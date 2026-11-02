import io
import json
import os
from pathlib import Path
import plistlib
import queue
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import traceback
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import profile_scenario
import scenario


class ScenarioTest(unittest.TestCase):
    def test_profile_harness_uses_the_isolated_scheduler_path(self) -> None:
        with tempfile.TemporaryDirectory() as folder, patch("profile_scenario.Client") as client:
            root = Path(folder)
            binary = root / "rindcut"
            harness = profile_scenario.Harness(binary, root)
            self.assertEqual(harness.environment["PATH"].split(os.pathsep)[0], str(root / "scheduler-bin"))
            client.assert_called_once_with(binary, harness.environment)

    def test_scheduler_commands_stay_inside_the_fixture(self) -> None:
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            home = root / "home"
            environment = scenario.scheduler_environment(root, home)
            for command in ("launchctl", "systemctl"):
                self.assertEqual(Path(shutil.which(command, path=environment["PATH"])).parent, root / "scheduler-bin")
                result = subprocess.run([command, "unsupported-private-command"], env=environment,
                                        capture_output=True, text=True, timeout=5, check=False)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")
                self.assertEqual(result.stderr, "unsupported simulated scheduler request\n")
            calls = [json.loads(line) for line in (root / "scheduler-calls.jsonl").read_text().splitlines()]
            self.assertEqual([call["command"] for call in calls], ["launchctl", "systemctl"])

    def test_simulated_launchctl_reports_only_loaded_fixture_state(self) -> None:
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            environment = scenario.scheduler_environment(root, root / "home")
            target = "gui/501/dev.rindcut.tick"

            def run(*arguments: str) -> subprocess.CompletedProcess:
                return subprocess.run(["launchctl", *arguments], env=environment,
                                      capture_output=True, text=True, timeout=5, check=False)

            self.assertEqual(run("print", target).returncode, 113)
            path = root / "tick.plist"
            config = {"Label": "dev.rindcut.tick", "ProgramArguments": [sys.executable, "tick"],
                      "EnvironmentVariables": {"HOME": str(root), "RINDCUT_HOME": str(root / "home")}}
            path.write_bytes(plistlib.dumps(config))
            self.assertEqual(run("bootstrap", "gui/501", str(path)).returncode, 0)
            loaded = run("print", target)
            self.assertEqual(loaded.returncode, 0)
            self.assertIn("path = " + str(path), loaded.stdout)
            self.assertIn("program = " + sys.executable, loaded.stdout)
            self.assertIn("HOME => " + str(root), loaded.stdout)
            self.assertIn('"dev.rindcut.tick" => enabled', run("print-disabled", "gui/501").stdout)
            self.assertEqual(run("bootout", target).returncode, 0)
            self.assertEqual(run("print", target).returncode, 113)

    def test_simulated_systemctl_reports_only_fixture_units(self) -> None:
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            home = root / "home"
            environment = scenario.scheduler_environment(root, home)

            def run(*arguments: str) -> subprocess.CompletedProcess:
                return subprocess.run(["systemctl", "--user", *arguments], env=environment,
                                      capture_output=True, text=True, timeout=5, check=False)

            properties = "--property=LoadState,ActiveState,UnitFileState,FragmentPath,DropInPaths"
            self.assertIn("LoadState=not-found", run("show", "rindcut-tick.timer", properties, "--all").stdout)
            units = root / ".config" / "systemd" / "user"
            units.mkdir(parents=True)
            (units / "rindcut-tick.timer").write_text("[Timer]\nOnCalendar=*:0/5\n", encoding="utf-8")
            service = "\n".join(("[Service]", "Environment=" + json.dumps("HOME=" + str(root)),
                                 "Environment=" + json.dumps("RINDCUT_HOME=" + str(home)),
                                 "ExecStart=" + json.dumps(sys.executable) + " tick", ""))
            (units / "rindcut-tick.service").write_text(service, encoding="utf-8")
            self.assertEqual(run("daemon-reload").returncode, 0)
            self.assertEqual(run("enable", "--now", "rindcut-tick.timer").returncode, 0)
            timer = run("show", "rindcut-tick.timer", properties, "--all")
            self.assertEqual(timer.returncode, 0)
            self.assertIn("ActiveState=active", timer.stdout)
            self.assertIn("UnitFileState=enabled", timer.stdout)
            self.assertIn("FragmentPath=" + str(units / "rindcut-tick.timer"), timer.stdout)
            properties = "--property=LoadState,FragmentPath,DropInPaths,ExecStart,Environment"
            loaded = run("show", "rindcut-tick.service", properties, "--all")
            self.assertIn("path=" + sys.executable, loaded.stdout)
            self.assertIn("HOME=" + str(root), loaded.stdout)
            self.assertEqual(run("disable", "--now", "rindcut-tick.timer").returncode, 0)

    def test_reader_terminal_failures_reach_the_caller(self) -> None:
        cases = (("", "MCP stream closed"),
                 ("private-invalid-json\n", "MCP stream returned invalid JSON"),
                 ("null\n", "MCP stream returned non-object JSON"),
                 ("[]\n", "MCP stream returned non-object JSON"))
        for wire, message in cases:
            with self.subTest(wire=wire):
                client = scenario.Client.__new__(scenario.Client)
                client.sequence = 0
                client.messages = queue.Queue()
                client.process = SimpleNamespace(stdout=io.StringIO(wire))
                escaped = []
                with patch("threading.excepthook", side_effect=escaped.append), patch.object(client, "send"):
                    reader = threading.Thread(target=client.read)
                    reader.start()
                    reader.join(timeout=1)
                    self.assertFalse(reader.is_alive())
                    take = client.messages.get
                    with patch.object(client.messages, "get", side_effect=lambda **_: take(block=False)):
                        with self.assertRaisesRegex(RuntimeError, "^" + message + "$") as raised:
                            client.request("tools/call", {"name": "source_save", "arguments": {}})
                    self.assertNotIn("private-invalid-json", str(raised.exception))
                self.assertEqual(escaped, [])

    def test_pipe_failures_reach_the_caller_without_the_response_timeout(self) -> None:
        spawn = subprocess.Popen
        program = '''import json
import sys
for line in sys.stdin:
    request = json.loads(line)
    if request.get("method") == "initialize":
        print(json.dumps({"id": request["id"], "result": {}}), flush=True)
    if request.get("method") == "tools/call":
        if sys.argv[1] == "invalid":
            print("private-invalid-json", flush=True)
        break
'''
        for kind, message in (("eof", "MCP stream closed"), ("invalid", "MCP stream returned invalid JSON")):
            with self.subTest(kind=kind):
                def launch(_command: list[str], **kwargs: object) -> subprocess.Popen:
                    return spawn([sys.executable, "-B", "-c", program, kind], **kwargs)

                with patch("scenario.subprocess.Popen", side_effect=launch):
                    client = scenario.Client(Path("fixture-binary"), {"PATH": os.defpath})
                try:
                    started = time.monotonic()
                    with self.assertRaisesRegex(RuntimeError, "^" + message + "$") as raised:
                        client.call("source_save", {})
                    self.assertLess(time.monotonic() - started, 5)
                    self.assertNotIn("private-invalid-json", str(raised.exception))
                finally:
                    client.close()
                self.assertIsNotNone(client.process.poll())
                self.assertFalse(client.reader.is_alive())

    def test_reader_io_failure_uses_a_fixed_diagnostic(self) -> None:
        client = scenario.Client.__new__(scenario.Client)
        client.sequence = 0
        client.messages = queue.Queue()
        stream = io.StringIO("private-output")
        stream.close()
        client.process = SimpleNamespace(stdout=stream)
        client.read()
        with patch.object(client, "send"):
            with self.assertRaisesRegex(RuntimeError, "^MCP stream read failed$"):
                client.request("test", {})

    def test_valid_response_precedes_eof_and_terminal_failure_stays_visible(self) -> None:
        client = scenario.Client.__new__(scenario.Client)
        client.sequence = 0
        client.messages = queue.Queue()
        stream = io.StringIO()
        stream.close()
        client.process = SimpleNamespace(stdin=stream, stdout=io.StringIO('{"id":1,"result":{"ready":true}}\n'))
        client.read()
        self.assertEqual(client.request("first", {}), {"ready": True})
        for _ in range(2):
            with self.assertRaisesRegex(RuntimeError, "^MCP stream closed$"):
                client.request("later", {})

    def test_exited_child_preserves_its_response_and_fixed_terminal_diagnostic(self) -> None:
        spawn = subprocess.Popen
        program = '''import json
import sys
for line in sys.stdin:
    request = json.loads(line)
    if request.get("method") == "initialize":
        print(json.dumps({"id": request["id"], "result": {}}), flush=True)
    elif request.get("method") == "tools/call":
        print(json.dumps({"id": request["id"], "result": {"ready": True}}), flush=True)
    elif request.get("method") == "finish":
        if sys.argv[1] == "invalid":
            print("private-invalid-json", flush=True)
        elif sys.argv[1] == "non-object":
            print("null", flush=True)
        elif sys.argv[1] == "io":
            sys.stdout.buffer.write(bytes([255, 10]))
            sys.stdout.buffer.flush()
        sys.stderr.write("private-child-stderr")
        break
'''
        cases = (("eof", "MCP stream closed"),
                 ("invalid", "MCP stream returned invalid JSON"),
                 ("non-object", "MCP stream returned non-object JSON"),
                 ("io", "MCP stream read failed"))
        for kind, message in cases:
            with self.subTest(kind=kind):
                def launch(_command: list[str], **kwargs: object) -> subprocess.Popen:
                    return spawn([sys.executable, "-B", "-c", program, kind], **kwargs)

                with patch("scenario.subprocess.Popen", side_effect=launch):
                    client = scenario.Client(Path("fixture-binary"), {"PATH": os.defpath, "PYTHONIOENCODING": "utf-8"})
                try:
                    self.assertEqual(client.request("tools/call", {}), {"ready": True})
                    client.send({"method": "finish"})
                    self.assertEqual(client.process.wait(timeout=5), 0)
                    client.reader.join(timeout=5)
                    self.assertFalse(client.reader.is_alive())
                    started = time.monotonic()
                    for _ in range(2):
                        with self.assertRaisesRegex(RuntimeError, "^" + message + "$") as raised:
                            client.request("later", {})
                        diagnostic = "".join(traceback.format_exception(raised.exception))
                        for value in ("private-invalid-json", "private-child-stderr", "BrokenPipeError", "UnicodeDecodeError"):
                            self.assertNotIn(value, diagnostic)
                    self.assertLess(time.monotonic() - started, 1)
                finally:
                    client.close()
                self.assertIsNotNone(client.process.poll())
                self.assertFalse(client.reader.is_alive())

    def test_write_failure_without_a_terminal_message_is_fixed_and_immediate(self) -> None:
        client = scenario.Client.__new__(scenario.Client)
        client.sequence = 0
        client.messages = queue.Queue()
        stream = io.StringIO()
        stream.close()
        client.process = SimpleNamespace(stdin=stream)
        started = time.monotonic()
        for _ in range(2):
            with self.assertRaisesRegex(RuntimeError, "^MCP stream write failed$") as raised:
                client.request("test", {"value": "private-request"})
            self.assertIsNone(raised.exception.__cause__)
            self.assertTrue(raised.exception.__suppress_context__)
        self.assertLess(time.monotonic() - started, 1)

    def test_blocked_write_shares_the_request_deadline_and_reaps_the_child(self) -> None:
        spawn = subprocess.Popen
        program = 'import time; print(\'{"id":1,"result":{}}\', flush=True); time.sleep(30)'

        def launch(_command: list[str], **kwargs: object) -> subprocess.Popen:
            return spawn([sys.executable, "-B", "-c", program], **kwargs)

        with patch("scenario.subprocess.Popen", side_effect=launch):
            client = scenario.Client(Path("fixture-binary"), {"PATH": os.defpath})
        guard = threading.Timer(1, client.process.terminate)
        guard.start()
        monotonic = time.monotonic
        started = monotonic()
        clock_started = False

        def clock() -> float:
            nonlocal clock_started
            if not clock_started:
                clock_started = True
                return 0
            return 19.9 + monotonic() - started

        try:
            with patch("scenario.time.monotonic", side_effect=clock):
                with self.assertRaisesRegex(TimeoutError, "^MCP response timed out$"):
                    client.request("test", {"value": "x" * (4 * 1024 * 1024)})
            self.assertLess(time.monotonic() - started, 0.5)
            with patch.object(client, "send", side_effect=AssertionError("timed-out stream was reused")) as send:
                with self.assertRaisesRegex(TimeoutError, "^MCP response timed out$"):
                    client.request("later", {})
                send.assert_not_called()
        finally:
            guard.cancel()
            client.process.terminate()
            client.close()
        self.assertIsNotNone(client.process.poll())
        self.assertFalse(client.reader.is_alive())

    def test_queued_response_and_terminal_precede_an_expired_write_deadline(self) -> None:
        client = scenario.Client.__new__(scenario.Client)
        client.sequence = 0
        client.messages = queue.Queue()
        client.messages.put({"id": 1, "result": {"ready": True}})
        client.messages.put(RuntimeError("MCP stream closed"))
        with patch.object(client, "send", side_effect=TimeoutError("MCP response timed out")):
            with patch("scenario.time.monotonic", return_value=0):
                self.assertEqual(client.request("first", {}), {"ready": True})
            with patch("scenario.time.monotonic", side_effect=[0, 21]):
                with self.assertRaisesRegex(RuntimeError, "^MCP stream closed$"):
                    client.request("later", {})

    def test_initialization_failure_reaps_the_child(self) -> None:
        spawn = subprocess.Popen
        for target in ("scenario.Client.request", "scenario.Client.send", "scenario.threading.Thread.start"):
            with self.subTest(target=target):
                children = []

                def launch(_command: list[str], **kwargs: object) -> subprocess.Popen:
                    child = spawn([sys.executable, "-c", "import sys; sys.stdin.read()"], **kwargs)
                    children.append(child)
                    return child

                try:
                    with patch("scenario.subprocess.Popen", side_effect=launch), patch(target, side_effect=RuntimeError("init failed")):
                        with self.assertRaisesRegex(RuntimeError, "init failed"):
                            scenario.Client(Path("fixture-binary"), {"PATH": os.defpath})
                    self.assertEqual(1, len(children))
                    self.assertIsNotNone(children[0].poll(), "failed initialization left an MCP child alive")
                finally:
                    for child in children:
                        if child.poll() is None:
                            child.kill()
                        child.wait(timeout=5)
                        for stream in (child.stdin, child.stdout):
                            stream.close()

    def test_spawn_failure_preserves_the_original_error(self) -> None:
        with patch("scenario.subprocess.Popen", side_effect=OSError("spawn failed")):
            with self.assertRaisesRegex(OSError, "spawn failed"):
                scenario.Client(Path("fixture-binary"), {})

    def test_close_reaps_after_a_broken_input_pipe(self) -> None:
        client = scenario.Client.__new__(scenario.Client)
        client.process = Mock()
        client.process.stdin.close.side_effect = BrokenPipeError("closed")
        client.process.wait.side_effect = [subprocess.TimeoutExpired("fixture-binary", 5), 0]
        client.close()
        client.process.kill.assert_called_once_with()
        self.assertEqual(2, client.process.wait.call_count)

    def test_request_has_one_deadline_even_with_notifications(self) -> None:
        client = scenario.Client.__new__(scenario.Client)
        client.sequence = 0
        client.messages = Mock()
        client.messages.get.side_effect = [{"method": "notification"}, queue.Empty()]
        with patch.object(client, "send"), patch("scenario.time.monotonic", side_effect=[0, 0, 21]):
            with self.assertRaisesRegex(TimeoutError, "MCP response timed out"):
                client.request("test", {})
        self.assertEqual(1, client.messages.get.call_count)

    def test_readonly_query_rejects_repeated_cursors_and_unbounded_pages(self) -> None:
        for repeated in (True, False):
            with self.subTest(repeated=repeated), tempfile.TemporaryDirectory() as folder:
                harness = profile_scenario.Harness.__new__(profile_scenario.Harness)
                harness.home = Path(folder)
                harness.folder = Path(folder)
                harness.client = Mock()
                page = {}
                reads = 0

                def next_page(*_args: object) -> dict:
                    nonlocal reads
                    reads += 1
                    if reads > (3 if repeated else 1001):
                        raise RuntimeError("test guard stopped an unbounded query")
                    page.update(records=[], next_cursor="same" if repeated else str(reads),
                                has_more=True, store_present=True)
                    return page

                def cli(*_args: str, as_json: bool = True) -> object:
                    if as_json:
                        return page
                    return json.dumps({"kind": "page", **{key: page[key] for key in
                                      ("next_cursor", "has_more", "store_present")}}) + "\n"

                harness.client.call.side_effect = next_page
                with patch.object(harness, "calls_so_far", return_value=[]), patch.object(harness, "cli", side_effect=cli):
                    with self.assertRaises(AssertionError):
                        harness.readonly_queries(["acme"])
                self.assertLessEqual(reads, 1000)


if __name__ == "__main__":
    unittest.main()
