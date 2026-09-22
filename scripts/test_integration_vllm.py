"""GPU-free tests for the optional integration runner (Python standard library)."""

import argparse
import copy
import io
import json
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import integration_vllm as integration


class Response(io.BytesIO):
    status = 200

    def getheader(self, name, default=None):
        return "text/event-stream" if name == "Content-Type" else default


class ValidationTests(unittest.TestCase):
    model = "test-model"

    def completion(self):
        return {
            "id": "chatcmpl-test", "object": "chat.completion", "model": self.model,
            "choices": [{"message": {"role": "assistant", "content": "Hello!"}, "finish_reason": "stop"}],
            "usage": {"completion_tokens": 2},
        }

    def event(self, content=None, finish=None, model=None):
        return b"data: " + json.dumps({
            "object": "chat.completion.chunk", "model": model or self.model,
            "choices": [{"index": 0, "delta": {"content": content}, "finish_reason": finish}],
        }).encode() + b"\n\n"

    def test_completion(self):
        self.assertEqual(integration.validate_completion(self.completion(), self.model), "Hello!")
        for update in [
            {"object": "wrong"}, {"model": "wrong"}, {"id": ""}, {"choices": []},
            {"usage": {"completion_tokens": 0}}, {"usage": {"completion_tokens": integration.MAX_TOKENS + 1}},
            {"choices": [{"message": {"role": "assistant", "content": " "}, "finish_reason": "stop"}]},
            {"choices": [{"message": {"role": "assistant", "content": "Hello"}, "finish_reason": None}]},
        ]:
            with self.subTest(update=update), self.assertRaises(RuntimeError):
                result = copy.deepcopy(self.completion())
                result.update(update)
                integration.validate_completion(result, self.model)

    def test_stream(self):
        stream = self.event("Hello") + self.event("!", "stop") + b"data: [DONE]\n\n"
        self.assertEqual(integration.validate_stream(Response(stream), self.model), ("Hello!", 2))

    def test_invalid_streams(self):
        for name, stream in {
            "empty": b"",
            "missing done": self.event("Hello", "stop"),
            "missing finish": self.event("Hello") + b"data: [DONE]\n\n",
            "no text": self.event(None, "stop") + b"data: [DONE]\n\n",
            "wrong model": self.event("Hello", "stop", "other") + b"data: [DONE]\n\n",
            "after done": self.event("Hello", "stop") + b"data: [DONE]\n\n" + self.event("extra"),
            "invalid JSON": b"data: {broken}\n\n",
            "long line": b"data: " + b"x" * (64 * 1024),
        }.items():
            with self.subTest(name=name), self.assertRaises((RuntimeError, json.JSONDecodeError)):
                integration.validate_stream(Response(stream), self.model)

    def test_timeout_arguments(self):
        for value in ["0", "-1", "nan", "inf"]:
            with self.subTest(value=value), self.assertRaises(argparse.ArgumentTypeError):
                integration.positive_seconds(value)
        self.assertEqual(integration.positive_seconds("1.5"), 1.5)

    def test_ports_are_distinct(self):
        first, second = integration.choose_ports()
        self.assertNotEqual(first, second)
        self.assertGreater(first, 0)
        self.assertGreater(second, 0)


class ProcessTests(unittest.TestCase):
    def test_stop_signals_owned_groups_and_escalates(self):
        with tempfile.TemporaryDirectory() as directory:
            processes = integration.Processes(Path(directory))
            parent = mock.Mock(pid=12345)
            parent.wait.side_effect = [subprocess.TimeoutExpired("test", 10), 0]
            processes.children["vllm"] = parent
            with mock.patch.object(integration.os, "killpg") as kill:
                processes.stop()
                self.assertEqual(kill.call_args_list, [
                    mock.call(12345, signal.SIGTERM), mock.call(12345, signal.SIGKILL),
                ])
                processes.stop()  # Never signal a reaped process group's ID twice.
                self.assertEqual(kill.call_count, 2)

    def test_cleanup_still_signals_group_of_exited_parent(self):
        with tempfile.TemporaryDirectory() as directory:
            processes = integration.Processes(Path(directory))
            processes.children["vllm"] = mock.Mock(pid=12345, returncode=1)
            with mock.patch.object(integration.os, "killpg") as kill:
                processes.stop()
                self.assertEqual(kill.call_args_list[0], mock.call(12345, signal.SIGTERM))

    def test_real_child_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            processes = integration.Processes(Path(directory))
            child = processes.start("child", [sys.executable, "-c", "import time; time.sleep(60)"])
            try:
                processes.check()
            finally:
                processes.stop()
            self.assertIsNotNone(child.poll())

    def test_startup_reports_early_exit(self):
        with tempfile.TemporaryDirectory() as directory:
            processes = integration.Processes(Path(directory))
            child = processes.start("child", [sys.executable, "-c", "raise SystemExit(7)"])
            try:
                child.wait(timeout=5)
                with self.assertRaisesRegex(RuntimeError, "child exited unexpectedly"):
                    integration.wait_ready(processes, 1, 10)
            finally:
                processes.stop()


if __name__ == "__main__":
    unittest.main()
