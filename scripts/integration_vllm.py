#!/usr/bin/env python3
"""Opt-in real inference test. Uses only Python's standard library; installs nothing."""

import argparse
from contextlib import contextmanager
import http.client
import json
import math
import os
from pathlib import Path
import secrets
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parent.parent
DEFAULT_MODEL = "mlx-community/Qwen2.5-0.5B-Instruct-4bit"
MAX_TOKENS = 32
MAX_RESPONSE_BYTES = 1 << 20


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def default_vllm():
    # An activated environment takes precedence. The fallback is the environment
    # used by the local `vac` alias; never source a user's interactive shell files.
    return os.environ.get("VLLM_BIN") or shutil.which("vllm") or str(
        Path.home() / ".venv-vllm-metal/bin/vllm"
    )


def positive_seconds(value):
    number = float(value)
    if not math.isfinite(number) or number <= 0:
        raise argparse.ArgumentTypeError("timeout must be a positive finite number")
    return number


def parse_args(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--vllm", default=default_vllm(), help="vLLM executable (or VLLM_BIN)")
    parser.add_argument("--model", default=DEFAULT_MODEL, help="Hugging Face model ID or local path")
    parser.add_argument("--startup-timeout", type=positive_seconds, default=600,
                        help="seconds per server startup, including downloads (default: 600)")
    parser.add_argument("--request-timeout", type=positive_seconds, default=120,
                        help="network I/O timeout in seconds (default: 120)")
    parser.add_argument("--keep-logs", action="store_true", help="retain artifacts even on success")
    return parser.parse_args(argv)


def choose_ports():
    # Hold both reservations together so the two ports cannot be identical.
    # The servers cannot inherit these sockets, so validate their identity after
    # binding as well: a process on a raced port must never produce a false pass.
    with socket.socket() as backend, socket.socket() as gateway:
        backend.bind(("127.0.0.1", 0))
        gateway.bind(("127.0.0.1", 0))
        return backend.getsockname()[1], gateway.getsockname()[1]


@contextmanager
def response(port, path, payload=None, timeout=120):
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=timeout)
    try:
        body = None if payload is None else json.dumps(payload).encode()
        connection.request("GET" if body is None else "POST", path, body=body,
                           headers={} if body is None else {"Content-Type": "application/json"})
        yield connection.getresponse()
    finally:
        connection.close()


def json_request(port, path, payload=None, timeout=120, status=200):
    with response(port, path, payload, timeout) as res:
        body = res.read(MAX_RESPONSE_BYTES + 1)
        require(len(body) <= MAX_RESPONSE_BYTES, "response exceeds 1 MiB")
        require(res.status == status, f"{path}: expected HTTP {status}, got {res.status}: {body[:500]!r}")
        require(res.getheader("Content-Type", "").split(";")[0] == "application/json",
                f"{path}: expected JSON Content-Type")
        return json.loads(body)


def completion_payload(model, stream=False):
    return {
        "model": model,
        "messages": [{"role": "user", "content": "Say hello in one short sentence."}],
        "temperature": 0,
        "max_tokens": MAX_TOKENS,
        "stream": stream,
    }


def validate_completion(result, model):
    require(result.get("object") == "chat.completion", "not a chat completion")
    require(result.get("model") == model, "completion returned the wrong model")
    require(bool(result.get("id")), "completion has no ID")
    choices = result.get("choices", [])
    require(len(choices) == 1, "expected one completion choice")
    message = choices[0].get("message", {})
    require(message.get("role") == "assistant", "completion has no assistant message")
    text = message.get("content")
    require(isinstance(text, str) and bool(text.strip()), "completion returned no text")
    require(choices[0].get("finish_reason") in ("stop", "length"), "completion did not finish")
    tokens = result.get("usage", {}).get("completion_tokens", 0)
    require(isinstance(tokens, int) and 0 < tokens <= MAX_TOKENS, "invalid completion token count")
    return text


def validate_stream(res, model):
    require(res.status == 200, f"stream returned HTTP {res.status}")
    require(res.getheader("Content-Type", "").split(";")[0] == "text/event-stream",
            "stream is not text/event-stream")
    text = []
    finished = False
    done = False
    events = 0
    total_bytes = 0
    # Read events as they arrive rather than buffering the entire response.
    # Small byte/event limits catch accidental unbounded output independently
    # of the model's max_tokens limit.
    while True:
        line = res.readline(64 * 1024 + 1)
        if not line:
            break
        total_bytes += len(line)
        require(total_bytes <= MAX_RESPONSE_BYTES and len(line) <= 64 * 1024,
                "stream exceeded response size limit")
        line = line.strip()
        if not line or line.startswith(b":"):
            continue
        require(not done, "stream contains data after [DONE]")
        require(line.startswith(b"data:"), f"unexpected SSE line: {line[:100]!r}")
        data = line[5:].strip()
        if data == b"[DONE]":
            done = True
            continue
        chunk = json.loads(data)
        events += 1
        require(events <= 256, "too many stream events")
        require(chunk.get("object") == "chat.completion.chunk", "invalid stream object")
        require(chunk.get("model") == model, "stream returned the wrong model")
        for choice in chunk.get("choices", []):
            require(choice.get("index") == 0, "unexpected stream choice")
            content = choice.get("delta", {}).get("content")
            if content is not None:
                require(isinstance(content, str), "stream content is not text")
                text.append(content)
            reason = choice.get("finish_reason")
            if reason is not None:
                require(reason in ("stop", "length"), f"unexpected finish reason: {reason}")
                finished = True
    require(done and finished, "stream ended without finish_reason and [DONE]")
    require(bool("".join(text).strip()), "stream returned no text")
    return "".join(text), events


class Processes:
    """Own process groups, including vLLM's engine subprocesses."""

    def __init__(self, directory):
        self.directory = directory
        self.children = {}
        self.stopped = set()

    def start(self, name, command):
        with (self.directory / f"{name}.log").open("wb") as log:
            child = subprocess.Popen(command, cwd=ROOT, stdout=log, stderr=subprocess.STDOUT,
                                     start_new_session=True)
        self.children[name] = child
        return child

    def check(self):
        for name, child in self.children.items():
            if name not in self.stopped:
                require(child.poll() is None, f"{name} exited unexpectedly; see {name}.log")

    def stop(self, names=None):
        names = [name for name in (names or list(self.children)) if name not in self.stopped]
        children = [self.children[name] for name in names]
        # Signal whole owned groups, even if a parent already died. Never use
        # pkill, port-based kills, or process-name matching.
        for child in children:
            self.signal_group(child, signal.SIGTERM)
        deadline = time.monotonic() + 10
        try:
            for child in children:
                try:
                    child.wait(timeout=max(0, deadline - time.monotonic()))
                except subprocess.TimeoutExpired:
                    pass
        finally:
            for child in children:
                self.signal_group(child, signal.SIGKILL)
            for child in children:
                child.wait(timeout=5)
            self.stopped.update(names)

    @staticmethod
    def signal_group(child, sig):
        try:
            os.killpg(child.pid, sig)
        except ProcessLookupError:
            pass


def wait_ready(processes, port, timeout):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        processes.check()
        try:
            with response(port, "/health", timeout=min(1, max(0.01, deadline - time.monotonic()))) as res:
                if res.status == 200:
                    processes.check()
                    return
        except (OSError, http.client.HTTPException):
            pass
        time.sleep(min(0.2, max(0, deadline - time.monotonic())))
    raise RuntimeError(f"server on port {port} was not ready within {timeout:g}s")


def run(args):
    vllm = shutil.which(args.vllm)
    require(vllm is not None, "vLLM executable not found; activate your environment or set VLLM_BIN")
    vllm = str(Path(vllm).resolve())
    require(shutil.which("go") is not None, "Go is required to build the gateway")
    directory = Path(tempfile.mkdtemp(prefix="vire-vllm-"))
    processes = Processes(directory)
    success = False
    print(f"Model: {args.model}\nvLLM: {vllm}\nStartup logs: {directory}", flush=True)
    print("The first run downloads model weights into the Hugging Face cache.", flush=True)
    try:
        executable = directory / "gateway"
        subprocess.run(["go", "build", "-o", str(executable), "./cmd/gateway"], cwd=ROOT,
                       check=True, timeout=120)
        backend_port, gateway_port = choose_ports()
        model = "vire-test-" + secrets.token_hex(4)
        registry = directory / "models.json"
        backend_url = f"http://127.0.0.1:{backend_port}"
        registry.write_text(json.dumps([{"name": model, "url": backend_url}]))
        processes.start("vllm", [vllm, "serve", args.model, "--host", "127.0.0.1",
                                "--port", str(backend_port), "--served-model-name", model,
                                "--max-model-len", "1024", "--max-num-seqs", "1",
                                "--max-num-batched-tokens", "1024", "--gpu-memory-utilization", "0.1"])
        print("Waiting for real vLLM (download, model loading, and kernel compilation)...", flush=True)
        wait_ready(processes, backend_port, args.startup_timeout)
        models = json_request(backend_port, "/v1/models", timeout=args.request_timeout)
        require(model in [item.get("id") for item in models.get("data", [])],
                "backend identity mismatch: expected our unique served-model-name")

        started = time.monotonic()
        result = json_request(backend_port, "/v1/chat/completions", completion_payload(model), args.request_timeout)
        print(f"PASS direct vLLM ({time.monotonic() - started:.2f}s): {validate_completion(result, model)!r}", flush=True)

        processes.start("gateway", [str(executable), "-addr", f"127.0.0.1:{gateway_port}", "-registry", str(registry), "-insecure-dev"])
        wait_ready(processes, gateway_port, args.startup_timeout)
        require(json_request(gateway_port, "/models", timeout=args.request_timeout) ==
                [{"name": model, "url": backend_url}], "gateway registry identity mismatch")
        started = time.monotonic()
        result = json_request(gateway_port, "/v1/chat/completions", completion_payload(model), args.request_timeout)
        print(f"PASS gateway completion ({time.monotonic() - started:.2f}s): {validate_completion(result, model)!r}", flush=True)
        with response(gateway_port, "/v1/chat/completions", completion_payload(model, True), args.request_timeout) as res:
            text, events = validate_stream(res, model)
        print(f"PASS gateway SSE ({events} events, finish_reason, [DONE]): {text!r}", flush=True)
        error = json_request(gateway_port, "/v1/chat/completions", completion_payload("not-a-registered-model"),
                             args.request_timeout, status=404)
        require(bool(error.get("error")), "unknown model did not return a JSON error")
        print("PASS unknown model returns 404", flush=True)
        processes.check()
        processes.stop(["vllm"])
        error = json_request(gateway_port, "/v1/chat/completions", completion_payload(model),
                             args.request_timeout, status=502)
        require(bool(error.get("error")), "stopped backend did not return a JSON error")
        with response(gateway_port, "/health", timeout=args.request_timeout) as res:
            require(res.status == 200, "gateway liveness failed after backend shutdown")
        print("PASS stopped backend returns 502; gateway remains healthy", flush=True)
        success = True
    finally:
        processes.stop()
        if success and not args.keep_logs:
            shutil.rmtree(directory)
        else:
            print(f"Artifacts retained: {directory}", file=sys.stderr, flush=True)
            if not success:
                for name in processes.children:
                    log = directory / f"{name}.log"
                    print(f"--- {name}.log (last 30 lines) ---", file=sys.stderr)
                    print("\n".join(log.read_text(errors="replace").splitlines()[-30:]), file=sys.stderr)
    print("PASS real-vLLM integration; owned processes stopped.", flush=True)


def main():
    def interrupted(_signum, _frame):
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, interrupted)
    try:
        run(parse_args())
    except KeyboardInterrupt:
        print("Integration test interrupted.", file=sys.stderr)
        return 130
    except Exception as exc:
        print(f"FAIL: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
