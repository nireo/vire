# vire

A small Go gateway for an inference platform. Currently supports static routing to
vLLM-compatible HTTP backends; worker provisioning and scale-to-zero are not yet
implemented.

## GPU-free development (macOS or Linux)

Requires the Go version in `go.mod`, Bash, and curl. No GPU, Python, or Docker is
needed.

```sh
make dev
```

This builds and starts two local processes:

```text
client → gateway (127.0.0.1:8080) → fake vLLM (127.0.0.1:8000)
```

The fake backend returns deterministic text; it does **not** run a model. The
supervisor waits for readiness, reports child failures, and stops both processes
on Ctrl-C. Binaries and a development registry live in a temporary directory and
are removed on exit; your `models.json` is not changed.

In another terminal:

```sh
make smoke

curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"example","messages":[{"role":"user","content":"Hello"}],"stream":true}'
```

Omit `stream` or set it to `false` for a normal JSON completion.

### Simulate delays and failures

```sh
STARTUP_DELAY=2s CHUNK_DELAY=300ms make dev
RESPONSE_DELAY=1s make dev
ERROR_STATUS=503 make dev
GATEWAY_PORT=8081 BACKEND_PORT=8001 make dev
GATEWAY_URL=http://127.0.0.1:8081 make smoke
```

- `STARTUP_DELAY`: time until the fake backend's health/completions become ready.
- `RESPONSE_DELAY`: wait before sending completion headers, including errors.
- `CHUNK_DELAY`: delay between SSE chunks (default `100ms`).
- `ERROR_STATUS`: force completion responses to a 4xx/5xx status (`0` disables).
- `READINESS_TIMEOUT`: maximum seconds to wait for each process (default `30`).

The delays are cancelable. The fake backend logs canceled requests without
logging prompt content. `make smoke` expects a healthy fake backend; it should
fail when an error mode is enabled or a response exceeds its timeout.

Run the fake backend independently with:

```sh
go run ./cmd/fake-vllm -addr 127.0.0.1:8000 -model example -chunk-delay 100ms
```

## Use a real vLLM backend

Run vLLM separately on a supported machine, for example:

```sh
vllm serve <model-id> --served-model-name example --host 127.0.0.1 --port 8000
```

Set the registry URL to an origin reachable from the gateway (an SSH tunnel can
keep a remote backend private). Names must match vLLM's served model name:

```json
[
  {"name": "example", "url": "http://localhost:8000"}
]
```

Then start only the gateway:

```sh
go run ./cmd/gateway -addr 127.0.0.1:8080 -registry models.json
```

URLs must use HTTP(S) and contain no credentials, path prefix, query, or fragment;
a trailing `/` is allowed. Model names must be unique. One backend per model is
supported. An empty registry is valid.

### HTTP behavior

- `GET /health`: gateway liveness, independent of backend availability.
- `GET /models`: existing registry response (includes backend URLs; not OpenAI's
  `/v1/models` format).
- `POST /v1/chat/completions`: selects a backend using `model`, then forwards the
  original JSON bytes. Unknown request fields are left to the backend.
- JSON and SSE responses retain upstream status, body, and end-to-end headers.
  SSE is flushed as it arrives; responses are not buffered in full.
- Client disconnects cancel the upstream HTTP request. Whether computation stops
  immediately also depends on the real backend.
- No application-level retries or redirects: a generation may already have been
  accepted upstream. Failures after streaming starts terminate the stream rather
  than appending a new JSON error or synthesizing `[DONE]`.

Gateway-generated errors use a JSON `error` object:

| Status | Cause |
| --- | --- |
| 400 | Invalid JSON, missing/non-string model, or unreadable body |
| 404 | Unknown model |
| 413 | Request body exceeds 1 MiB |
| 502 | Backend connection/transport failure |
| 504 | Backend transport timeout |

Requests are buffered up to 1 MiB for model lookup. Header reads are limited to
5 seconds, request reads to 30 seconds, and upstream response-header waits to
2 minutes. There is no whole-response write timeout. Shutdown drains requests
for up to 5 seconds, then closes remaining connections.

**Development only:** there is no authentication, tenant isolation, or concurrency
admission limit yet. Do not expose this gateway publicly. The development stack
binds to loopback; the standalone gateway's existing default is `:8080`.
Registry destinations are trusted configuration. Client authorization headers
are passed upstream; forwarded-address headers from clients are stripped.

## Tests

```sh
make test       # deterministic, GPU-free tests
make check      # non-mutating formatting check, race tests, go vet
make build
make fmt        # explicitly apply Go formatting
```

Gateway integration tests use `httptest` backends and actual HTTP connections.
They cover routing, original-body preservation, validation and size limits,
upstream errors, connection failures, header timeouts, incremental SSE delivery,
and cancellation before/during streaming. The fake backend has its own tests.
`make smoke` checks the separately running development stack.

These tests validate HTTP/orchestration behavior, not vLLM compatibility in full,
inference quality, or GPU performance. A pinned real-vLLM integration suite is a
future addition when GPU infrastructure is available.
