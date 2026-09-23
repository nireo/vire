#!/usr/bin/env bash
# Own the two child processes; never kill processes merely because they use a port.
set -euo pipefail
cd "$(dirname "$0")/.."

GATEWAY_PORT=${GATEWAY_PORT:-8080}
BACKEND_PORT=${BACKEND_PORT:-8000}
METRICS_PORT=${METRICS_PORT:-9091}
READINESS_TIMEOUT=${READINESS_TIMEOUT:-30}
for port in "$GATEWAY_PORT" "$BACKEND_PORT" "$METRICS_PORT"; do
    if [[ ! "$port" =~ ^[1-9][0-9]{0,4}$ ]] || (( port > 65535 )); then
        echo "Ports must be integers between 1 and 65535" >&2
        exit 1
    fi
done
if [[ "$GATEWAY_PORT" == "$BACKEND_PORT" || "$GATEWAY_PORT" == "$METRICS_PORT" || "$BACKEND_PORT" == "$METRICS_PORT" ]]; then
    echo "Gateway, backend, and metrics must use different ports" >&2
    exit 1
fi
if [[ ! "$READINESS_TIMEOUT" =~ ^[1-9][0-9]{0,3}$ ]]; then
    echo "READINESS_TIMEOUT must be an integer between 1 and 9999 seconds" >&2
    exit 1
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/vire-dev.XXXXXX")
backend_pid=
gateway_pid=
cleanup() {
    local status=$?
    trap - EXIT INT TERM
    for pid in "$gateway_pid" "$backend_pid"; do
        if [[ -n "$pid" ]]; then kill -TERM "$pid" 2>/dev/null || true; fi
    done
    local deadline=$((SECONDS + 6))
    while (( SECONDS < deadline )); do
        local alive=false
        for pid in "$gateway_pid" "$backend_pid"; do
            if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then alive=true; fi
        done
        if [[ "$alive" == false ]]; then break; fi
        sleep 0.1
    done
    for pid in "$gateway_pid" "$backend_pid"; do
        if [[ -n "$pid" ]]; then
            kill -KILL "$pid" 2>/dev/null || true
            wait "$pid" 2>/dev/null || true
        fi
    done
    rm -rf "$tmp"
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

check_children() {
    for pid in "$backend_pid" "$gateway_pid"; do
        if [[ -n "$pid" ]] && ! kill -0 "$pid" 2>/dev/null; then
            local status=0
            wait "$pid" || status=$?
            echo "Development process $pid exited unexpectedly (status $status)" >&2
            return 1
        fi
    done
}

wait_ready() {
    local url=$1
    local deadline=$((SECONDS + READINESS_TIMEOUT))
    while (( SECONDS < deadline )); do
        check_children || return 1
        if curl --noproxy '*' --fail --silent --max-time 1 "$url" >/dev/null; then
            # A port collision must not be mistaken for our child becoming ready.
            check_children || return 1
            return 0
        fi
        sleep 0.1
    done
    echo "Timed out waiting for $url after ${READINESS_TIMEOUT}s" >&2
    return 1
}

echo "Building gateway and fake vLLM..."
go build -o "$tmp/" ./cmd/gateway ./cmd/fake-vllm
printf '[{"name":"example","url":"http://127.0.0.1:%s"}]\n' "$BACKEND_PORT" > "$tmp/models.json"

"$tmp/fake-vllm" -addr "127.0.0.1:$BACKEND_PORT" \
    -startup-delay "${STARTUP_DELAY:-0s}" \
    -response-delay "${RESPONSE_DELAY:-0s}" \
    -chunk-delay "${CHUNK_DELAY:-100ms}" \
    -error-status "${ERROR_STATUS:-0}" &
backend_pid=$!
wait_ready "http://127.0.0.1:$BACKEND_PORT/health"

"$tmp/gateway" -addr "127.0.0.1:$GATEWAY_PORT" -registry "$tmp/models.json" \
	-metrics-addr "127.0.0.1:$METRICS_PORT" -insecure-dev &
gateway_pid=$!
wait_ready "http://127.0.0.1:$GATEWAY_PORT/health"

echo "Ready: gateway http://127.0.0.1:$GATEWAY_PORT → fake vLLM http://127.0.0.1:$BACKEND_PORT"
echo "Metrics: http://127.0.0.1:$METRICS_PORT/metrics"
echo "Run 'make smoke' in another terminal. Ctrl-C stops both processes."
while true; do
    check_children
    sleep 0.2
done
