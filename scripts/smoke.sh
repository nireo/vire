#!/usr/bin/env bash
set -euo pipefail
base_url=${GATEWAY_URL:-http://127.0.0.1:8080}
tmp=$(mktemp -d "${TMPDIR:-/tmp}/vire-smoke.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

curl --noproxy '*' --fail --silent --show-error --max-time 5 "$base_url/health" >/dev/null
curl --noproxy '*' --fail --silent --show-error --max-time 15 \
    -H 'Content-Type: application/json' \
    -d '{"model":"example","messages":[{"role":"user","content":"Hello"}]}' \
    "$base_url/v1/chat/completions" > "$tmp/completion.json"
grep -q '"object"[[:space:]]*:[[:space:]]*"chat.completion"' "$tmp/completion.json"

curl --noproxy '*' --fail --silent --show-error --no-buffer --max-time 15 \
    -H 'Content-Type: application/json' \
    -d '{"model":"example","messages":[{"role":"user","content":"Hello"}],"stream":true}' \
    "$base_url/v1/chat/completions" > "$tmp/stream.txt"
grep -q '"object"[[:space:]]*:[[:space:]]*"chat.completion.chunk"' "$tmp/stream.txt"
grep -q '^data: \[DONE\]$' "$tmp/stream.txt"
echo "Smoke passed: health, completion, and SSE stream through $base_url"
