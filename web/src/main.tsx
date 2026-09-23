import { createMemo, createResource, createSignal, For, onMount, Show } from "solid-js";
import { render } from "solid-js/web";
import { ChatView } from "./chat";
import "./style.css";

type Signup = { account_id: string; key_id: string; api_key: string };
type Account = { account_id: string; name: string; email: string };
type Model = {
  id: string;
  display_name: string;
  pricing: { input_usd_per_million: number; output_usd_per_million: number } | null;
};
type Usage = {
  day: string; model: string; requests: number; complete: number; incomplete: number; failed: number;
  prompt_tokens: number; completion_tokens: number; estimated_cost_micro: number; priced: number;
};
type ApiError = { error?: { message?: string } };

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", "'\\''")}'`;
}

async function getJSON<T>(path: string): Promise<T> {
  const response = await fetch(path);
  if (!response.ok) throw new Error(`Request failed (${response.status})`);
  return response.json() as Promise<T>;
}

async function postJSON<T>(path: string, data: object): Promise<T> {
  const response = await fetch(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) });
  if (!response.ok) {
    const body = (await response.json().catch(() => ({}))) as ApiError;
    throw new Error(body.error?.message || `Request failed (${response.status})`);
  }
  return response.status === 204 ? (undefined as T) : (response.json() as Promise<T>);
}

function App() {
  const [path, setPath] = createSignal(location.pathname);
  const [models] = createResource(async () => (await getJSON<{ models: Model[] }>("/api/models")).models);
  const [selectedModelID, setSelectedModelID] = createSignal("");
  const selectedModel = createMemo(() => models()?.find((model) => model.id === selectedModelID()) ?? models()?.[0]);
  const examplePayload = createMemo(() => ({
    model: selectedModel()?.id ?? "",
    messages: [{ role: "user", content: "Hello" }],
    max_tokens: 64,
  }));
  const exampleRequest = createMemo(() => JSON.stringify(examplePayload(), null, 2));
  const curlExample = createMemo(() => `curl ${shellQuote(`${location.origin}/v1/chat/completions`)} \\
  -H 'Content-Type: application/json' \\
  -H 'Authorization: Bearer YOUR_API_KEY' \\
  -d ${shellQuote(JSON.stringify(examplePayload()))}`);
  const [curlCopied, setCurlCopied] = createSignal(false);
  const [curlCopyError, setCurlCopyError] = createSignal(false);
  const [account, { refetch: refreshAccount }] = createResource(async () => {
    const response = await fetch("/api/account");
    if (response.status === 401) return null;
    if (!response.ok) throw new Error("Account is unavailable.");
    return (await response.json()) as Account;
  });
  const [usage, { refetch: refreshUsage }] = createResource(
    () => (path() === "/account" && account() ? account()!.account_id : undefined),
    async () => (await getJSON<{ usage: Usage[] }>("/api/usage")).usage,
  );
  const [mode, setMode] = createSignal<"signup" | "login">("signup");
  const [name, setName] = createSignal("");
  const [email, setEmail] = createSignal("");
  const [password, setPassword] = createSignal("");
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal("");
  const [signup, setSignup] = createSignal<Signup>();
  const [copied, setCopied] = createSignal(false);
  const totals = createMemo(() => (usage() || []).reduce((sum, row) => ({
    requests: sum.requests + row.requests,
    prompt: sum.prompt + row.prompt_tokens,
    completion: sum.completion + row.completion_tokens,
    cost: sum.cost + row.estimated_cost_micro,
    priced: sum.priced + row.priced,
  }), { requests: 0, prompt: 0, completion: 0, cost: 0, priced: 0 }));

  onMount(() => {
    if (location.pathname === "/account") setMode("login");
    const update = () => setPath(location.pathname);
    window.addEventListener("popstate", update);
    return () => window.removeEventListener("popstate", update);
  });

  function go(next: string) {
    history.pushState(null, "", next);
    setPath(next);
    if (next === "/account" && !account()) setMode("login");
    setError("");
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    setError(""); setSubmitting(true);
    try {
      if (mode() === "signup") {
        const result = await postJSON<Signup>("/api/signup", { name: name().trim(), email: email().trim(), password: password() });
        setSignup(result);
      } else {
        await postJSON("/api/login", { email: email().trim(), password: password() });
        setSignup(undefined);
      }
      setPassword("");
      await refreshAccount();
      go("/account");
      await refreshUsage();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Could not sign in.");
    } finally { setSubmitting(false); }
  }

  async function logout() {
    setError("");
    try {
      await postJSON("/api/logout", {});
      setSignup(undefined);
      await refreshAccount();
      go("/");
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Could not sign out."); }
  }

  async function copyKey() {
    const key = signup()?.api_key;
    if (!key) return;
    try { await navigator.clipboard.writeText(key); setCopied(true); }
    catch { setError("Copy failed. Select the key and copy it manually."); }
  }

  async function copyCurl() {
    try {
      await navigator.clipboard.writeText(curlExample());
      setCurlCopied(true);
      setCurlCopyError(false);
    } catch {
      setCurlCopyError(true);
    }
  }

  const authForm = () => <form onSubmit={submit}>
    <Show when={mode() === "signup"}>
      <label for="name">Account name</label>
      <input id="name" name="name" type="text" autocomplete="organization" maxlength="100" required value={name()} onInput={(event) => setName(event.currentTarget.value)} placeholder="Your team or project" />
    </Show>
    <label for="email">Email address</label>
    <input id="email" name="email" type="email" autocomplete="email" maxlength="254" required value={email()} onInput={(event) => setEmail(event.currentTarget.value)} placeholder="you@example.com" />
    <label for="password">Password</label>
    <input id="password" name="password" type="password" autocomplete={mode() === "signup" ? "new-password" : "current-password"} minlength={mode() === "signup" ? "10" : undefined} maxlength="256" required value={password()} onInput={(event) => setPassword(event.currentTarget.value)} />
    <Show when={mode() === "signup"}><p class="form-note">Use at least 10 characters. Your API key will be shown once after signup.</p></Show>
    <Show when={error()}><p class="status-error" role="alert">{error()}</p></Show>
    <button class="primary-button" type="submit" disabled={submitting()}>{submitting() ? "Working…" : mode() === "signup" ? "Create account" : "Sign in"}</button>
  </form>;

  return <div class="site-shell">
    <header class="site-header">
      <div class="site-header-inner">
        <a class="brand" href="/" onClick={(event) => { event.preventDefault(); go("/"); }} aria-label="Vire home">vire</a>
        <nav class="header-nav" aria-label="Main navigation">
          <a href="/" aria-current={path() === "/" ? "page" : undefined} onClick={(event) => { event.preventDefault(); go("/"); }}>Models</a>
          <a href="/chat" aria-current={path() === "/chat" ? "page" : undefined} onClick={(event) => { event.preventDefault(); go("/chat"); }}>Chat</a>
          <a href="/account" aria-current={path() === "/account" ? "page" : undefined} onClick={(event) => { event.preventDefault(); go("/account"); }}>Account</a>
        </nav>
      </div>
    </header>
    <main>
      <div hidden={path() !== "/chat"}>
        <Show when={!account.loading} fallback={<p class="muted">Loading account…</p>}>
          <Show when={account()} fallback={<section class="chat-signin"><h1>Chat</h1><p>Sign in to chat with an available model.</p><button class="primary-button" type="button" onClick={() => go("/account")}>Sign in</button></section>}>
            {(current) => <ChatView models={models() ?? []} accountID={current().account_id} />}
          </Show>
        </Show>
      </div>
      <Show when={path() !== "/chat"}>
      <Show when={path() === "/account"} fallback={<>
        <section class="intro" aria-labelledby="page-title"><h1 id="page-title">Models and access</h1><p>Browse available models and create an account to get an API key.</p></section>
        <section class="content-section" aria-labelledby="models-title">
          <h2 id="models-title">Available models</h2>
          <Show when={!models.loading} fallback={<p class="muted">Loading models…</p>}>
            <Show when={!models.error} fallback={<p class="status-error" role="alert">Model list is unavailable. Refresh to try again.</p>}>
              <Show when={(models()?.length ?? 0) > 0} fallback={<p class="muted">No models are listed yet.</p>}>
                <div class="model-picker" role="group" aria-label="Select a model"><For each={models()}>{(model) =>
                  <button class="model-option" classList={{ selected: selectedModel()?.id === model.id }} type="button" aria-pressed={selectedModel()?.id === model.id} onClick={() => { setSelectedModelID(model.id); setCurlCopied(false); setCurlCopyError(false); }}>
                    <span class="model-option-main"><span class="model-option-name">{model.display_name}</span><code>{model.id}</code></span>
                    <span class="model-option-pricing">{model.pricing ? <>
                      <span class="model-option-rate"><small>Input / 1M</small><strong>${model.pricing.input_usd_per_million.toFixed(2)}</strong></span>
                      <span class="model-option-rate"><small>Output / 1M</small><strong>${model.pricing.output_usd_per_million.toFixed(2)}</strong></span>
                    </> : <span class="model-option-price-note">Pricing unavailable</span>}</span>
                  </button>
                }</For></div>
                <Show when={selectedModel()}>{(model) => <div class="model-details">
                  <h3>{model().display_name}</h3>
                  <p class="muted">Use <code>{model().id}</code> as the model ID in API requests.</p>
                  <div class="model-rates">
                    <div><span>Input</span><strong>{model().pricing ? `$${model().pricing!.input_usd_per_million.toFixed(2)}` : "—"}</strong><small>per 1M tokens</small></div>
                    <div><span>Output</span><strong>{model().pricing ? `$${model().pricing!.output_usd_per_million.toFixed(2)}` : "—"}</strong><small>per 1M tokens</small></div>
                  </div>
                  <p class="form-note">{model().pricing ? "Usage costs are estimates based on measured tokens." : "A price has not been configured for this model."}</p>
                  <h4>Request body</h4>
                  <pre class="request-example"><code>{exampleRequest()}</code></pre>
                  <div class="example-heading"><h4>curl example</h4><button class="text-button" type="button" onClick={copyCurl}>{curlCopied() ? "Copied" : "Copy"}</button></div>
                  <pre class="request-example"><code>{curlExample()}</code></pre>
                  <p class="form-note">Replace <code>YOUR_API_KEY</code> with your API key before running the command.</p>
                  <Show when={curlCopyError()}><p class="status-error" role="alert">Copy failed. Select the command and copy it manually.</p></Show>
                </div>}</Show>
              </Show>
            </Show>
          </Show>
        </section>
        <section class="content-section" aria-labelledby="access-title">
          <h2 id="access-title">Account</h2>
          <Show when={account()} fallback={<>
            <div class="tabs"><button type="button" classList={{ active: mode() === "signup" }} onClick={() => { setMode("signup"); setError(""); }}>Create account</button><button type="button" classList={{ active: mode() === "login" }} onClick={() => { setMode("login"); setError(""); }}>Sign in</button></div>
            {authForm()}
          </>}><p class="muted">Signed in as {account()?.email}. <a href="/account" onClick={(event) => { event.preventDefault(); go("/account"); }}>View account and usage</a>.</p></Show>
        </section>
      </>}>
        <section class="intro" aria-labelledby="page-title"><h1 id="page-title">Account</h1><p>Account details and usage from the last 30 days.</p></section>
        <Show when={!account.loading} fallback={<p class="muted">Loading account…</p>}>
          <Show when={account()} fallback={<section class="content-section"><h2>Sign in</h2><Show when={account.error}><p class="status-error" role="alert">Account is unavailable. Refresh to try again.</p></Show><div class="tabs"><button type="button" classList={{ active: mode() === "login" }} onClick={() => setMode("login")}>Sign in</button><button type="button" classList={{ active: mode() === "signup" }} onClick={() => setMode("signup")}>Create account</button></div>{authForm()}</section>}>
            {(current) => <>
              <section class="content-section account-details"><div><h2>{current().name}</h2><p class="muted">{current().email}</p></div><button class="secondary-button" type="button" onClick={logout}>Sign out</button></section>
              <Show when={signup()}>{(result) => <section class="content-section success" aria-live="polite"><h2>Your API key</h2><p>Save this key now. It is shown only once.</p><div class="key-box"><code>{result().api_key}</code></div><button class="secondary-button" type="button" onClick={copyKey}>{copied() ? "Copied" : "Copy API key"}</button></section>}</Show>
              <section class="content-section"><div class="section-heading"><h2>Usage · last 30 days</h2><button class="text-button" type="button" onClick={() => refreshUsage()} disabled={usage.loading}>Refresh</button></div>
                <Show when={!usage.loading} fallback={<p class="muted">Loading usage…</p>}>
                  <Show when={!usage.error} fallback={<p class="status-error" role="alert">Usage is unavailable. Refresh to try again.</p>}>
                    <div class="usage-summary"><div><span>Requests</span><strong>{totals().requests.toLocaleString()}</strong></div><div><span>Input tokens</span><strong>{totals().prompt.toLocaleString()}</strong></div><div><span>Output tokens</span><strong>{totals().completion.toLocaleString()}</strong></div><div><span>Estimated cost</span><strong>{totals().priced === totals().requests ? `$${(totals().cost / 1_000_000).toFixed(4)}` : "—"}</strong></div></div>
                    <Show when={(usage()?.length ?? 0) > 0} fallback={<p class="muted">No completed requests yet.</p>}>
                      <div class="table-scroll"><table><thead><tr><th>Day</th><th>Model</th><th>Requests</th><th>Input</th><th>Output</th><th>Est. cost</th></tr></thead><tbody><For each={usage()}>{(row) => <tr><td>{row.day.slice(0, 10)}</td><td>{row.model}</td><td>{row.requests}</td><td>{row.prompt_tokens}</td><td>{row.completion_tokens}</td><td>{row.priced === row.requests ? `$${(row.estimated_cost_micro / 1_000_000).toFixed(4)}` : "—"}</td></tr>}</For></tbody></table></div>
                    </Show>
                    <p class="form-note">Cost appears when every request has a configured model price. Incomplete requests may have no token counts.</p>
                  </Show>
                </Show>
              </section>
              <Show when={error()}><p class="status-error" role="alert">{error()}</p></Show>
            </>}
          </Show>
        </Show>
      </Show>
      </Show>
    </main>
  </div>;
}

render(() => <App />, document.getElementById("root")!);
