import { createResource, createSignal, For, Show } from "solid-js";
import { render } from "solid-js/web";
import "./style.css";

type Signup = {
  account_id: string;
  key_id: string;
  api_key: string;
};

type ApiError = { error?: { message?: string } };

async function loadModels(): Promise<string[]> {
  const response = await fetch("/api/models");
  if (!response.ok) throw new Error("Model list is unavailable.");
  const data = (await response.json()) as { models: string[] };
  return data.models;
}

function App() {
  const [models] = createResource(loadModels);
  const [name, setName] = createSignal("");
  const [email, setEmail] = createSignal("");
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal("");
  const [signup, setSignup] = createSignal<Signup>();
  const [copied, setCopied] = createSignal(false);

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    setError("");
    setSubmitting(true);
    try {
      const response = await fetch("/api/signup", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name: name().trim(), email: email().trim() }),
      });
      if (!response.ok) {
        const body = (await response.json()) as ApiError;
        throw new Error(body.error?.message || "Could not create account.");
      }
      setSignup((await response.json()) as Signup);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Could not create account.");
    } finally {
      setSubmitting(false);
    }
  }

  async function copyKey() {
    const key = signup()?.api_key;
    if (!key) return;
    try {
      await navigator.clipboard.writeText(key);
      setCopied(true);
    } catch {
      setError("Copy failed. Select the key and copy it manually.");
    }
  }

  return (
    <div class="site-shell">
      <header class="site-header">
        <a class="brand" href="/" aria-label="Vire home">vire</a>
        <span class="header-label">API portal</span>
      </header>

      <main>
        <section class="intro" aria-labelledby="page-title">
          <h1 id="page-title">Models and access</h1>
          <p>Browse available models and create an account to get an API key.</p>
        </section>

        <section class="content-section" aria-labelledby="models-title">
          <h2 id="models-title">Available models</h2>
          <Show when={!models.loading} fallback={<p class="muted">Loading models…</p>}>
            <Show when={!models.error} fallback={<p class="status-error" role="alert">Model list is unavailable. Refresh to try again.</p>}>
              <Show when={(models()?.length ?? 0) > 0} fallback={<p class="muted">No models are listed yet.</p>}>
                <ul class="model-list">
                  <For each={models()}>{(model) => <li>{model}</li>}</For>
                </ul>
              </Show>
            </Show>
          </Show>
        </section>

        <section class="content-section" aria-labelledby="signup-title">
          <h2 id="signup-title">Create an account</h2>
          <Show when={signup()} fallback={
            <form onSubmit={submit}>
              <label for="name">Account name</label>
              <input id="name" name="name" type="text" autocomplete="organization" maxlength="100" required value={name()} onInput={(event) => setName(event.currentTarget.value)} placeholder="Your team or project" />
              <label for="email">Email address</label>
              <input id="email" name="email" type="email" autocomplete="email" maxlength="254" required value={email()} onInput={(event) => setEmail(event.currentTarget.value)} placeholder="you@example.com" />
              <p class="form-note">Your API key is shown once after signup. Save it somewhere secure.</p>
              <Show when={error()}><p class="status-error" role="alert">{error()}</p></Show>
              <button class="primary-button" type="submit" disabled={submitting()}>{submitting() ? "Creating account…" : "Create account"}</button>
            </form>
          }>
            {(result) => <div class="success" aria-live="polite">
              <h3>Account created</h3>
              <p>Save this API key now. It is shown only once.</p>
              <div class="key-box"><code>{result().api_key}</code></div>
              <button class="secondary-button" type="button" onClick={copyKey}>{copied() ? "Copied" : "Copy API key"}</button>
              <Show when={error()}><p class="status-error" role="alert">{error()}</p></Show>
            </div>}
          </Show>
        </section>
      </main>
    </div>
  );
}

render(() => <App />, document.getElementById("root")!);
