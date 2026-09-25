import { streamingMarkdownExtension } from "@tanstack/markdown/extensions/streaming";
import { renderHtml } from "@tanstack/markdown/html";
import { createEffect, createSignal, For, onCleanup, Show } from "solid-js";

type Model = { id: string; display_name: string };
type Turn = { role: "user" | "assistant"; content: string };
type Message = Turn & { id: number; status?: "streaming" | "stopped" | "error" };

const markdownOptions = {
  allowHtml: false,
  frontmatter: false,
  headingIds: false,
  extensions: [streamingMarkdownExtension()],
};

export function ChatView(props: { models: Model[]; accountID: string }) {
  const [modelID, setModelID] = createSignal("");
  const [draft, setDraft] = createSignal("");
  const [messages, setMessages] = createSignal<Message[]>([]);
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal("");
  let history: Turn[] = [];
  let conversationID = crypto.randomUUID();
  let nextID = 0;
  let controller: AbortController | undefined;
  let stopped = false;
  let frame = 0;
  let pendingText = "";
  let thread: HTMLDivElement | undefined;
  let composer: HTMLTextAreaElement | undefined;
  let previousAccount = props.accountID;
  let generation = 0;

  const selectedModel = () => modelID() || props.models[0]?.id || "";

  function updateReply(id: number, content: string, status?: Message["status"]) {
    const follow = thread && thread.scrollHeight - thread.scrollTop - thread.clientHeight < 100;
    setMessages((current) => current.map((item) => item.id === id ? { ...item, content, status } : item));
    if (follow) requestAnimationFrame(() => thread?.scrollTo({ top: thread.scrollHeight }));
  }

  function reset() {
    generation++;
    controller?.abort();
    controller = undefined;
    cancelAnimationFrame(frame);
    frame = 0;
    pendingText = "";
    history = [];
    conversationID = crypto.randomUUID();
    setMessages([]);
    setDraft("");
    if (composer) { composer.style.height = ""; composer.style.overflowY = ""; }
    setError("");
    setBusy(false);
    composer?.focus();
  }

  createEffect(() => {
    const account = props.accountID;
    if (previousAccount !== account) reset();
    previousAccount = account;
  });
  onCleanup(() => { controller?.abort(); cancelAnimationFrame(frame); });

  async function send(event: SubmitEvent) {
    event.preventDefault();
    const prompt = draft().trim();
    const model = selectedModel();
    if (!prompt || !model || busy() || !props.accountID) return;
    const user: Message = { id: ++nextID, role: "user", content: prompt };
    const replyID = ++nextID;
    const follow = thread && thread.scrollHeight - thread.scrollTop - thread.clientHeight < 100;
    setMessages((current) => [...current, user, { id: replyID, role: "assistant", content: "", status: "streaming" }]);
    if (follow) requestAnimationFrame(() => thread?.scrollTo({ top: thread.scrollHeight }));
    setDraft("");
    if (composer) { composer.style.height = ""; composer.style.overflowY = ""; }
    setError("");
    setBusy(true);
    stopped = false;
    const requestGeneration = generation;
    const requestController = new AbortController();
    controller = requestController;
    let answer = "";
    let complete = false;
    const flush = () => {
      frame = 0;
      if (requestGeneration !== generation) return;
      if (pendingText) {
        answer += pendingText;
        pendingText = "";
        updateReply(replyID, answer, "streaming");
      }
    };
    const accept = (chunk: string) => {
      if (requestGeneration !== generation) return;
      pendingText += chunk;
      if (!frame) frame = requestAnimationFrame(flush);
    };
    try {
      const response = await fetch("/api/chat/completions", {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-Conversation-ID": conversationID },
        body: JSON.stringify({ model, messages: [...history, { role: "user", content: prompt }], stream: true, stream_options: { include_usage: true } }),
        signal: requestController.signal,
      });
      if (!response.ok) {
        const body = await response.json().catch(() => ({}));
        throw new Error(body.error?.message || `Request failed (${response.status})`);
      }
      if (!response.body || !response.headers.get("Content-Type")?.includes("text/event-stream")) {
        throw new Error("The model did not return a stream.");
      }
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";
      const processEvent = (eventText: string) => {
        const data = eventText.split(/\r?\n/).filter((line) => line.startsWith("data:")).map((line) => line.slice(5).trimStart()).join("\n");
        if (!data) return;
        if (data === "[DONE]") { complete = true; return; }
        const event = JSON.parse(data);
        if (event.error) throw new Error(event.error.message || "Generation failed.");
        const content = event.choices?.[0]?.delta?.content;
        if (typeof content === "string") accept(content);
      };
      while (!complete) {
        const { value, done } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        let boundary: RegExpExecArray | null;
        while ((boundary = /\r?\n\r?\n/.exec(buffer))) {
          const eventText = buffer.slice(0, boundary.index);
          buffer = buffer.slice(boundary.index + boundary[0].length);
          processEvent(eventText);
          if (complete) break;
        }
      }
      if (!complete) throw new Error("The response ended before it was complete.");
      if (requestGeneration !== generation) return;
      cancelAnimationFrame(frame);
      flush();
      updateReply(replyID, answer);
      history = [...history, { role: "user", content: prompt }, { role: "assistant", content: answer }];
    } catch (cause) {
      if (requestGeneration !== generation) return;
      cancelAnimationFrame(frame);
      flush();
      if (stopped) {
        updateReply(replyID, answer, "stopped");
      } else {
        const message = cause instanceof Error ? cause.message : "Could not get a response.";
        setError(message);
        updateReply(replyID, answer, "error");
      }
    } finally {
      if (requestGeneration === generation) {
        controller = undefined;
        setBusy(false);
        composer?.focus();
      }
    }
  }

  function stop() {
    stopped = true;
    controller?.abort();
  }

  function resizeComposer() {
    if (!composer) return;
    composer.style.height = "auto";
    const contentHeight = composer.scrollHeight;
    composer.style.height = `${Math.min(Math.max(contentHeight, 44), 160)}px`;
    composer.style.overflowY = contentHeight > 160 ? "auto" : "hidden";
  }

  return <section class="chat-page" aria-label="Chat">
    <div class="chat-controls">
      <label for="chat-model">Model</label><select id="chat-model" value={selectedModel()} disabled={busy() || !props.models.length} onChange={(event) => { setModelID(event.currentTarget.value); reset(); }}>
        <For each={props.models}>{(model) => <option value={model.id}>{model.display_name}</option>}</For>
      </select>
      <button class="secondary-button" type="button" onClick={reset} disabled={!messages().length}>New chat</button>
    </div>
    <div class="chat-thread" ref={thread} role="log" aria-label="Conversation">
      <Show when={messages().length} fallback={<div class="chat-empty"><h2>What can I help with?</h2><p>Choose a model and send a message to start.</p></div>}>
        <For each={messages()}>{(message) => <article class={`chat-message ${message.role}`} aria-label={message.role === "user" ? "Your message" : "Assistant response"}>
          {message.role === "assistant" && message.content
            ? <div class="chat-content chat-markdown" innerHTML={renderHtml(message.content, markdownOptions)} />
            : <div class="chat-content">{message.content || (message.status === "streaming" ? <span class="chat-thinking">Thinking…</span> : "")}</div>}
          <Show when={message.status === "stopped"}><span class="chat-message-note">Stopped</span></Show>
          <Show when={message.status === "error"}><span class="chat-message-note">Response incomplete</span></Show>
        </article>}</For>
      </Show>
    </div>
    <div class="chat-footer">
      <Show when={error()}><p class="status-error" role="alert">{error()}</p></Show>
      <form class="chat-composer" onSubmit={send}>
        <label class="visually-hidden" for="chat-prompt">Message</label>
        <textarea id="chat-prompt" ref={composer} rows="1" value={draft()} onInput={(event) => { setDraft(event.currentTarget.value); resizeComposer(); }} onKeyDown={(event) => {
          if (event.key === "Enter" && !event.shiftKey && !event.isComposing) { event.preventDefault(); event.currentTarget.form?.requestSubmit(); }
        }} placeholder="Message the model…" disabled={busy() || !props.models.length} />
        <Show when={busy()} fallback={<button class="chat-action-button" type="submit" aria-label="Send message" title="Send message" disabled={!draft().trim() || !props.models.length}>
          <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 19V5m0 0-6 6m6-6 6 6" /></svg>
        </button>}>
          <button class="chat-action-button" type="button" onClick={stop} aria-label="Stop generating" title="Stop generating"><svg viewBox="0 0 24 24" aria-hidden="true"><rect x="7" y="7" width="10" height="10" rx="1" /></svg></button>
        </Show>
      </form>
    </div>
  </section>;
}
