#!/usr/bin/env python3
"""Run the real local stack with an isolated, disposable Postgres database."""

import json
import os
from pathlib import Path
import shlex
import shutil
import signal
import socket
import subprocess
import sys
import time
from urllib import error, request


ROOT = Path(__file__).resolve().parent.parent
STATE = Path(os.environ.get("VIRE_LOCAL_DIR", str(ROOT / ".vire-local"))).expanduser().resolve()
CONTROL = STATE / "control.sock"
DEFAULT_MODEL = "mlx-community/Qwen2.5-0.5B-Instruct-4bit"


def setting_int(name, default, maximum=65535):
    raw = os.environ.get(name, str(default))
    if not raw.isdecimal() or not 1 <= int(raw) <= maximum:
        raise RuntimeError(f"{name} must be an integer from 1 to {maximum}")
    return int(raw)


def executable(name):
    found = shutil.which(name)
    if not found:
        raise RuntimeError(f"{name} is required for make local-up")
    return found


def run_command(command, *, cwd=ROOT, env=None, log=None):
    if log:
        with log.open("wb") as output:
            result = subprocess.run(command, cwd=cwd, env=env, stdout=output,
                                    stderr=subprocess.STDOUT, check=False)
        if result.returncode:
            tail = log.read_text(errors="replace")[-3000:]
            raise RuntimeError(f"{' '.join(command[:2])} failed (see {log}):\n{tail}")
    else:
        subprocess.run(command, cwd=cwd, env=env, check=True)


def get_json(url):
    opener = request.build_opener(request.ProxyHandler({}))
    with opener.open(url, timeout=1) as response:
        return json.load(response)


def get_text(url):
    opener = request.build_opener(request.ProxyHandler({}))
    with opener.open(url, timeout=1) as response:
        return response.read().decode("utf-8")


def is_vite_page(url):
    html = get_text(url)
    return "Vire" in html and "@vite/client" in html


class Stack:
    def __init__(self):
        self.children = {}
        self.stop_requested = False
        self.control = None
        self.pg_ctl = None
        self.owns_state = False

    def signal_stop(self, _signum, _frame):
        self.stop_requested = True

    def start_child(self, name, command, env=None, cwd=ROOT):
        log = STATE / f"{name}.log"
        with log.open("wb") as output:
            child = subprocess.Popen(command, cwd=cwd, env=env, stdout=output,
                                     stderr=subprocess.STDOUT, start_new_session=True)
        self.children[name] = child

    def check_children(self):
        for name, child in self.children.items():
            status = child.poll()
            if status is not None:
                tail = (STATE / f"{name}.log").read_text(errors="replace")[-3000:]
                raise RuntimeError(f"{name} exited with status {status}:\n{tail}")

    def wait_ready(self, name, url, timeout, check):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            self.poll_control()
            if self.stop_requested:
                raise RuntimeError("startup interrupted")
            self.check_children()
            try:
                if check(url):
                    self.check_children()
                    return
            except (OSError, ValueError, error.URLError, error.HTTPError):
                pass
            time.sleep(0.25)
        log = STATE / f"{name}.log"
        tail = log.read_text(errors="replace")[-3000:] if log.exists() else ""
        raise RuntimeError(f"timed out waiting for {name} after {timeout}s:\n{tail}")

    def poll_control(self):
        try:
            connection, _ = self.control.accept()
        except socket.timeout:
            return
        with connection:
            if connection.recv(16).strip() == b"stop":
                self.stop_requested = True

    def start(self):
        backend_port = setting_int("VIRE_BACKEND_PORT", 8000)
        gateway_port = setting_int("VIRE_GATEWAY_PORT", 8080)
        web_port = setting_int("VIRE_WEB_PORT", 5173)
        postgres_port = setting_int("VIRE_POSTGRES_PORT", 15432)
        timeout = setting_int("VIRE_STARTUP_TIMEOUT", 600, 3600)
        if len({backend_port, gateway_port, web_port, postgres_port}) != 4:
            raise RuntimeError("backend, gateway, web, and Postgres ports must differ")
        model = os.environ.get("VIRE_MODEL", DEFAULT_MODEL)
        name = os.environ.get("VIRE_MODEL_NAME", "qwen2.5-0.5b-instruct" if model == DEFAULT_MODEL else model)
        if not model or not name:
            raise RuntimeError("VIRE_MODEL and VIRE_MODEL_NAME must be nonempty")
        vllm = os.environ.get("VIRE_VLLM_BIN") or shutil.which("vllm") or str(
            Path.home() / ".venv-vllm-metal/bin/vllm")
        vllm = executable(os.path.expanduser(vllm))
        self.pg_ctl = executable("pg_ctl")
        initdb = executable("initdb")
        createdb = executable("createdb")
        pnpm = executable("pnpm")
        go = executable("go")

        STATE.mkdir(mode=0o700)
        self.owns_state = True
        self.control = socket.socket(socket.AF_UNIX)
        self.control.bind(str(CONTROL))
        self.control.listen(1)
        self.control.settimeout(0.25)
        signal.signal(signal.SIGINT, self.signal_stop)
        signal.signal(signal.SIGTERM, self.signal_stop)

        print("Preparing the portal and gateway...", flush=True)
        if not (ROOT / "web/node_modules/.bin/vite").exists():
            install_env = os.environ.copy()
            install_env["CI"] = "true"
            run_command([pnpm, "install", "--frozen-lockfile"], cwd=ROOT / "web",
                        env=install_env, log=STATE / "pnpm-install.log")
        run_command([go, "build", "-o", str(STATE / "gateway"), "./cmd/gateway"], log=STATE / "go-build.log")
        self.poll_control()
        if self.stop_requested:
            return

        data = STATE / "postgres"
        run_command([initdb, "-D", str(data), "-U", "vire_local", "--auth-host=trust",
                     "--auth-local=trust"], log=STATE / "initdb.log")
        options = f"-h 127.0.0.1 -p {postgres_port} -k {shlex.quote(str(STATE))}"
        run_command([self.pg_ctl, "-D", str(data), "-l", str(STATE / "postgres.log"),
                     "-o", options, "-w", "start"], log=STATE / "pg-start.log")
        database_env = os.environ.copy()
        database_env.update(PGHOST="127.0.0.1", PGPORT=str(postgres_port), PGUSER="vire_local")
        run_command([createdb, "vire"], env=database_env, log=STATE / "createdb.log")
        database_env["VIRE_DATABASE_URL"] = (
            f"postgres://vire_local@127.0.0.1:{postgres_port}/vire?sslmode=disable")
        registry = STATE / "models.json"
        entry = {"name": name, "url": f"http://127.0.0.1:{backend_port}"}
        if model == DEFAULT_MODEL:
            entry["display_name"] = "Qwen 2.5 0.5B Instruct"
            if name == "qwen2.5-0.5b-instruct":
                entry["aliases"] = ["example"]
            entry["pricing"] = {"input_rate_micro_per_million": 100000,
                                "output_rate_micro_per_million": 200000,
                                "example": True}
        registry.write_text(json.dumps([entry]))

        print(f"Starting real vLLM ({model}); first use may download model weights...", flush=True)
        self.start_child("vllm", [vllm, "serve", model, "--host", "127.0.0.1",
                                  "--port", str(backend_port), "--served-model-name", name,
                                  "--max-model-len", "1024", "--max-num-seqs", "1",
                                  "--max-num-batched-tokens", "1024", "--gpu-memory-utilization", "0.1"])
        self.wait_ready("vllm", f"http://127.0.0.1:{backend_port}/v1/models", timeout,
                        lambda url: name in [item.get("id") for item in get_json(url).get("data", [])])

        self.start_child("gateway", [str(STATE / "gateway"), "-addr", f"127.0.0.1:{gateway_port}",
                                     "-registry", str(registry), "-metrics-addr=", "-insecure-cookies",
                                     "-portal-origin", f"http://127.0.0.1:{web_port}"], env=database_env)
        base = f"http://127.0.0.1:{gateway_port}"
        self.wait_ready("gateway", base + "/api/models", 30,
                        lambda url: [item.get("id") for item in get_json(url).get("models", [])] == [name])
        web_env = os.environ.copy()
        web_env.update(VIRE_GATEWAY_PORT=str(gateway_port), VIRE_WEB_PORT=str(web_port))
        self.start_child("web", [pnpm, "dev"], env=web_env, cwd=ROOT / "web")
        portal = f"http://127.0.0.1:{web_port}/"
        self.wait_ready("web", portal, 30, is_vite_page)
        print(f"Ready: portal {portal} (edits update automatically)", flush=True)
        print(f"API: {base}/v1  |  model: {name}", flush=True)
        print(f"Logs: {STATE}  |  Ctrl-C or 'make local-down' stops everything and removes the test database.", flush=True)

        next_database_check = 0
        while not self.stop_requested:
            self.check_children()
            if time.monotonic() >= next_database_check:
                status = subprocess.run([self.pg_ctl, "-D", str(data), "status"],
                                        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                        check=False)
                if status.returncode != 0:
                    raise RuntimeError("local Postgres stopped unexpectedly")
                next_database_check = time.monotonic() + 2
            self.poll_control()

    def cleanup(self):
        if not self.owns_state:
            return
        if self.control:
            self.control.close()
        for child in self.children.values():
            try:
                os.killpg(child.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
        deadline = time.monotonic() + 10
        for child in self.children.values():
            try:
                child.wait(timeout=max(0, deadline - time.monotonic()))
            except subprocess.TimeoutExpired:
                pass
            try:
                os.killpg(child.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            child.wait()
        data = STATE / "postgres"
        if self.pg_ctl and (data / "PG_VERSION").exists():
            status = subprocess.run([self.pg_ctl, "-D", str(data), "status"],
                                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
            if status.returncode == 0:
                subprocess.run([self.pg_ctl, "-D", str(data), "-m", "fast", "-w", "stop"],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
            status = subprocess.run([self.pg_ctl, "-D", str(data), "status"],
                                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
            if status.returncode == 0:
                print(f"Postgres is still running; preserving {STATE}", file=sys.stderr)
                return
        shutil.rmtree(STATE)


def up():
    stack = Stack()
    try:
        stack.start()
    finally:
        stack.cleanup()


def down():
    if not STATE.exists():
        print("Local stack is already stopped.")
        return
    deadline = time.monotonic() + 3
    while time.monotonic() < deadline:
        try:
            with socket.socket(socket.AF_UNIX) as connection:
                connection.connect(str(CONTROL))
                connection.sendall(b"stop\n")
            break
        except (FileNotFoundError, ConnectionRefusedError):
            time.sleep(0.1)
    else:
        raise RuntimeError("local stack control is unavailable; inspect .vire-local before removing it")
    deadline = time.monotonic() + 120
    while STATE.exists() and time.monotonic() < deadline:
        time.sleep(0.1)
    if STATE.exists():
        raise RuntimeError("local stack is still stopping; check its terminal and logs")
    print("Local stack stopped and disposable database removed.")


if __name__ == "__main__":
    try:
        if len(sys.argv) != 2 or sys.argv[1] not in {"up", "down"}:
            raise RuntimeError("usage: local_stack.py up|down")
        if sys.argv[1] == "up":
            up()
        else:
            down()
    except (OSError, RuntimeError, subprocess.CalledProcessError) as exc:
        print(f"local stack: {exc}", file=sys.stderr)
        sys.exit(1)
