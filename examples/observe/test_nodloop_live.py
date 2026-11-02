import copy
import importlib.util
import json
import os
import signal
from pathlib import Path
import selectors
import shutil
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

import nodloop_live as live


HERE = Path(__file__).resolve().parent
READER = HERE / "nodloop_live.py"
PAGE = {"records": [{"record_kind": "run", "record_id": "R1", "schema_version": 1,
                     "time": "2026-01-01T00:00:00Z", "input": "private prompt", "output": "private body",
                     "subject": "private subject", "session_id": "private session", "labels": ["private label"],
                     "reviewer": "private reviewer", "url": "https://example.com/private"}], "next": "QzE"}
EXPECTED = {"source_instance": "acme", "generation": "g1", "stream": "runs", "id": "R1",
            "type": "nodloop.run", "schema_version": "nodloop.raw/v1", "adapter_revision": "nodloop-live-v1",
            "projection_profile": "nodloop.public.metadata.v1", "action_id": "R1", "links": [],
            "occurred_at": "2026-01-01T00:00:00Z", "attributes": {"record_kind": "run", "native_schema_version": 1},
            "evidence": [{"kind": "source_record", "ref": {"namespace": "nodloop.records", "id": "R1"}}]}
SERVER = '''import json
from pathlib import Path
import os
import signal
import sys
import time
folder = Path(sys.argv[sys.argv.index("--record-dir") + 1])
settings = json.loads((folder / "page.json").read_text())
mode = settings.get("mode", "normal")
(folder / "native.pid").write_text(str(os.getpid()))
(folder / "native.pgid").write_text(str(os.getpgrp()))
if mode in ("orphan", "cancel", "stubborn", "descendant"):
    child = os.fork()
    if child == 0:
        time.sleep(30)
        os._exit(0)
    (folder / "child.pid").write_text(str(child))
    (folder / "native.ready").write_text("ready")
    if mode == "orphan":
        os._exit(0)
if mode == "cancel":
    time.sleep(30)
if mode == "stubborn":
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
for line in sys.stdin:
    request = json.loads(line)
    with (folder / "calls.jsonl").open("a") as calls:
        calls.write(json.dumps(request) + "\\n")
    if request.get("method") == "notifications/initialized":
        if mode == "blocked_write":
            time.sleep(2)
            break
        continue
    if mode == "eof":
        break
    if mode == "malformed":
        print("private invalid JSON", flush=True)
        continue
    if mode == "truncated":
        sys.stdout.write("{private")
        sys.stdout.flush()
        break
    if mode == "timeout":
        time.sleep(5)
    if mode == "oversized":
        print("x" * (2 * 1024 * 1024 + 1), flush=True)
        continue
    result = {"protocolVersion": "2025-06-18", "capabilities": {},
              "serverInfo": {"name": "nodloop", "version": "0.10.3"}}
    if request.get("method") == "tools/call":
        assert request["params"]["name"] == "records"
        assert request["params"]["arguments"]["content"] is False
        if mode == "cursor_invalid":
            result = {"isError": True, "content": [{"type": "text", "text": "schema: the cursor names no record"}]}
        elif mode == "tool_error":
            result = {"isError": True, "content": [{"type": "text", "text": "private provider error"}]}
        else:
            result = {"structuredContent": settings["page"], "content": []}
    print(json.dumps({"jsonrpc": "2.0", "id": request["id"], "result": result}), flush=True)
if mode == "stubborn":
    time.sleep(30)
'''

COLLECTOR = '''package command_test

import (
    "context"
    "encoding/json"
    "os"
    "testing"
    "time"

    "github.com/jeon-jihyeon/rindcut/internal/observe/command"
)

func TestNativeMCPGroup(t *testing.T) {
    var args []string
    if err := json.Unmarshal([]byte(os.Getenv("ACME_READER_ARGS")), &args); err != nil {
        t.Fatal(err)
    }
    prepared, err := command.Prepare(context.Background(), os.Getenv("ACME_PYTHON"), args, nil)
    if err != nil {
        t.Fatal(err)
    }
    duration := 5 * time.Second
    if os.Getenv("ACME_MODE") == "deadline" {
        duration = 2 * time.Second
    }
    ctx, cancel := context.WithTimeout(context.Background(), duration)
    defer cancel()
    if os.Getenv("ACME_MODE") == "cancel" {
        go func() {
            for {
                if _, err := os.Stat(os.Getenv("ACME_READY")); err == nil {
                    cancel()
                    return
                }
                select {
                case <-ctx.Done():
                    return
                case <-time.After(10 * time.Millisecond):
                }
            }
        }()
    }
    execution, runErr := command.Run(ctx, prepared, []byte(os.Getenv("ACME_REQUEST")))
    code := ""
    if runErr != nil {
        code = runErr.Error()
    }
    result, err := json.Marshal(map[string]any{
        "code": code, "started": execution.Started, "output": string(execution.Output),
    })
    if err != nil {
        t.Fatal(err)
    }
    if err = os.WriteFile(os.Getenv("ACME_RESULT"), result, 0600); err != nil {
        t.Fatal(err)
    }
}
'''


def request() -> dict:
    return {"protocol": "rindcut.source/v1", "operation": "read", "source_id": "nodloop",
            "source_revision": 1, "source_instance": "acme", "stream": "runs", "generation": None,
            "cursor": None, "limit": 1}


class NodloopLiveTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.folder = Path(self.temporary.name)
        self.program = self.folder / "nodloop"
        self.program.write_text("#!" + sys.executable + "\n" + SERVER, encoding="utf-8")
        self.program.chmod(0o700)
        self.page()
        self.reader = live.Reader(str(self.program), str(self.folder), "run", "acme", "g1", "runs", 2)

    def page(self, page: dict | None = None, mode: str = "normal") -> None:
        (self.folder / "page.json").write_text(json.dumps({"page": PAGE if page is None else page, "mode": mode}))

    def calls(self) -> list[dict]:
        path = self.folder / "calls.jsonl"
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def test_literal_projection_and_public_read_contract(self) -> None:
        actual = self.reader.read(request())
        self.assertEqual(actual["status"], "ok")
        self.assertEqual(actual["records"], [EXPECTED])
        self.assertTrue(actual["has_more"])
        calls = self.calls()
        self.assertEqual([item["method"] for item in calls], ["initialize", "notifications/initialized", "tools/call"])
        self.assertEqual(calls[-1]["params"], {"name": "records", "arguments": {
            "kind": "run", "since": "", "limit": 1, "content": False}})
        for private in ("private prompt", "private body", "private subject", "private session", "private label",
                        "private reviewer", "https://", "authorization", "verified"):
            self.assertNotIn(private, json.dumps(actual))

    def test_outcome_claims_never_become_observer_authority(self) -> None:
        record = {"record_kind": "outcome", "record_id": "O1", "schema_version": 1,
                  "trace_id": "R1", "time": "2026-01-01T00:00:00Z", "result": "refuted",
                  "actor": "code", "proof": "none", "note": "private body", "reviewer": "private reviewer"}
        self.page({"records": [record], "next": "QzE"})
        reader = live.Reader(str(self.program), str(self.folder), "outcome", "acme", "g1", "runs", 2)
        actual = reader.read(request())["records"][0]
        self.assertEqual(actual["attributes"], {"record_kind": "outcome", "native_schema_version": 1,
                         "declared_result": "refuted", "declared_actor": "code", "declared_proof": "none"})
        self.assertEqual(actual["action_id"], "R1")
        for field in ("authority", "verified", "result", "actor_kind", "target_ref", "criterion_id"):
            self.assertNotIn(field, actual)

    def test_cursor_replay_empty_page_and_late_record(self) -> None:
        first = self.reader.read(request())
        second = self.reader.read(request())
        self.assertEqual(first, second)
        query = dict(request(), generation="g1", cursor=first["next_cursor"])
        self.page({"records": [], "next": "QzE"})
        empty = self.reader.read(query)
        self.assertEqual(empty["next_cursor"], first["next_cursor"])
        self.assertEqual(empty["records"], [])
        self.assertFalse(empty["has_more"])
        self.assertEqual(self.calls()[-1]["params"]["arguments"]["since"], "QzE")
        late = copy.deepcopy(PAGE)
        late["records"][0].update(record_id="R2", time="2025-01-01T00:00:00Z")
        late["next"] = "QzI"
        self.page(late)
        self.assertEqual(self.reader.read(query)["records"][0]["id"], "R2")

    def test_scope_and_generation_changes_refuse_the_cursor_before_start(self) -> None:
        initial = self.reader.read(request())
        query = dict(request(), generation="g1", cursor=initial["next_cursor"])
        self.page({"records": [], "next": "QzE"})
        count = len(self.calls())
        changes = [{"kind": "outcome"}, {"instance": "other"}, {"generation": "g2"}, {"stream": "other"},
                   {"record_dir": str(self.folder / "other")}, {"program": str(self.folder / "other-program")}]
        for change in changes:
            with self.subTest(change=change):
                reader = live.Reader(str(self.program), str(self.folder), "run", "acme", "g1", "runs", 2)
                for name, value in change.items():
                    setattr(reader, name, value)
                result = reader.read(query)
                self.assertEqual(result["status"], "cursor_invalid")
                self.assertEqual(result["records"], [])
        self.assertEqual(self.reader.read(dict(query, source_id="other"))["status"], "cursor_invalid")
        self.assertEqual(self.reader.read(dict(query, generation="g2"))["status"], "cursor_invalid")
        with mock.patch.object(live, "REVISION", "nodloop-live-v2"):
            self.assertEqual(self.reader.read(query)["status"], "cursor_invalid")
        self.assertEqual(len(self.calls()), count)

    def test_native_invalid_cursor_is_not_silently_reset(self) -> None:
        self.page(mode="cursor_invalid")
        actual = self.reader.read(request())
        self.assertEqual(actual["status"], "cursor_invalid")
        self.assertIsNone(actual["next_cursor"])
        self.assertEqual(actual["records"], [])
        self.assertEqual(len(self.calls()), 3)

    def test_eof_invalid_json_timeout_and_bounds_have_fixed_diagnostics(self) -> None:
        cases = {"eof": "mcp_eof", "malformed": "mcp_invalid_json", "truncated": "mcp_invalid_json",
                 "timeout": "mcp_timeout", "oversized": "mcp_limit", "tool_error": "mcp_tool_error"}
        for mode, reason in cases.items():
            with self.subTest(mode=mode):
                self.page(mode=mode)
                self.reader.timeout = 0.4 if mode == "timeout" else 2
                actual = self.reader.read(request())
                self.assertEqual(actual["status"], "unavailable")
                self.assertIn(reason, actual["coverage"]["limitations"])
                self.assertEqual(actual["records"], [])
                self.assertIsNone(actual["next_cursor"])
                self.assertNotIn("private", json.dumps(actual))

    def test_invalid_native_values_and_future_versions_are_rejected(self) -> None:
        for name, value in (("record_kind", "outcome"), ("record_id", "https://example.com/private"),
                            ("schema_version", 2), ("schema_version", 3), ("schema_version", True), ("time", "invalid"),
                            ("actor", "has space")):
            with self.subTest(name=name, value=value):
                page = copy.deepcopy(PAGE)
                page["records"][0][name] = value
                self.page(page)
                with self.assertRaises(ValueError):
                    self.reader.read(request())

    def test_published_second_versions_keep_only_the_envelope(self) -> None:
        for kind in ("decide", "knob"):
            with self.subTest(kind=kind):
                page = copy.deepcopy(PAGE)
                page["records"][0].update(record_kind=kind, schema_version=2)
                self.page(page)
                reader = live.Reader(str(self.program), str(self.folder), kind, "acme", "g1", "runs", 2)
                event = reader.read(request())["records"][0]
                self.assertEqual(event["type"], "nodloop." + kind)
                self.assertEqual(event["schema_version"], "nodloop.raw/v2")
                self.assertEqual(event["attributes"], {"record_kind": kind, "native_schema_version": 2})

    def test_shutdown_reaps_its_own_process(self) -> None:
        client = live.MCP(str(self.program), str(self.folder), 2)
        with client:
            client.records("run", "", 1)
        self.assertIsNotNone(client.process.poll())

    def test_blocked_native_write_uses_the_existing_response_deadline(self) -> None:
        self.page(mode="blocked_write")
        client = live.MCP(str(self.program), str(self.folder), 0.5)
        started = time.monotonic()
        with client:
            with self.assertRaisesRegex(live.Failure, "^mcp_timeout$") as raised:
                client.send({"jsonrpc": "2.0", "method": "acme", "params": {"cursor": "x" * (2 << 20)}})
            self.assertTrue(raised.exception.__suppress_context__)
        self.assertLess(time.monotonic() - started, 1.5)
        self.assertIsNotNone(client.process.poll())

    def test_shutdown_kills_descendants_after_the_parent_exits(self) -> None:
        self.page(mode="orphan")
        sentinel = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(5)"],
                                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
        child = None
        try:
            actual = self.reader.read(request())
            self.assertEqual(actual["status"], "unavailable")
            child = int((self.folder / "child.pid").read_text())
            with self.assertRaises(ProcessLookupError):
                os.kill(child, 0)
            self.assertIsNone(sentinel.poll())
        finally:
            if child is not None:
                try:
                    os.kill(child, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            sentinel.terminate()
            sentinel.wait(timeout=3)

    def test_nonempty_page_requires_cursor_progress(self) -> None:
        self.page(dict(PAGE, next=""))
        with self.assertRaisesRegex(ValueError, "^native_cursor_stalled$"):
            self.reader.read(request())

    def test_definition_is_record_only_and_pins_the_native_program(self) -> None:
        definition = json.loads((HERE / "nodloop_live_definition.json").read_text(encoding="utf-8"))
        self.assertEqual(definition["adapter_revision"], "nodloop-live-v1")
        self.assertEqual(definition["projection_profile"], "nodloop.public.metadata.v1")
        self.assertEqual(definition["observers"], [])
        arguments = definition["reader"]["args"]
        program = arguments[arguments.index("--nodloop-program") + 1]
        self.assertIn(program, definition["reader"]["fingerprint_files"])
        self.assertIn("examples/observe/stdio.py", definition["reader"]["fingerprint_files"])
        self.assertEqual(definition["capability"]["types"], ["nodloop.run"])
        self.assertEqual(arguments.count("--managed-process-group"), 1)

    def test_invalid_request_never_starts_the_program(self) -> None:
        for query in (dict(request(), limit=True), dict(request(), source_revision=0),
                      dict(request(), url="https://example.com/private"), dict(request(), cursor="bad")):
            with self.subTest(query=query):
                if query.get("cursor") == "bad":
                    self.assertEqual(self.reader.read(query)["status"], "cursor_invalid")
                else:
                    with self.assertRaises(ValueError):
                        self.reader.read(query)
        self.assertEqual(self.calls(), [])

    def test_stdio_uses_only_registered_arguments_and_fixed_errors(self) -> None:
        arguments = [sys.executable, "-B", str(HERE / "nodloop_live.py"), "--nodloop-program", str(self.program),
                     "--record-dir", str(self.folder), "--kind", "run", "--source-instance", "acme",
                     "--generation", "g1", "--stream", "runs"]
        result = subprocess.run(arguments, input=json.dumps(request()), capture_output=True, text=True, timeout=5)
        self.assertEqual((result.returncode, result.stderr), (0, ""))
        self.assertEqual(json.loads(result.stdout)["records"], [EXPECTED])
        raw = json.dumps(request())[:-1] + ",\"source_id\":\"private payload\"}"
        result = subprocess.run(arguments, input=raw, capture_output=True, text=True, timeout=5)
        self.assertEqual((result.returncode, result.stdout, result.stderr), (1, "", "nodloop_live_failed\n"))
        result = subprocess.run(arguments + ["--private-payload"], input="{}",
                                capture_output=True, text=True, timeout=5)
        self.assertEqual((result.returncode, result.stdout, result.stderr), (1, "", "nodloop_live_failed\n"))

    def test_mutations_fail_independent_public_contract_assertions(self) -> None:
        original = (HERE / "nodloop_live.py").read_text(encoding="utf-8")
        cases = {
            "content_opt_in": ('"limit": limit, "content": False', '"limit": limit, "content": True',
                               "test_literal_projection_and_public_read_contract"),
            "blocking_native_write": ('stdio.write(self.process.stdin, adapter.encoded(message) + b"\\n", self.deadline)',
                                      'self.process.stdin.write(adapter.encoded(message) + b"\\n")\n'
                                      '            self.process.stdin.flush()',
                                      "test_blocked_native_write_uses_the_existing_response_deadline"),
            "cursor_scope": ('value["binding"] != binding', 'False',
                             "test_scope_and_generation_changes_refuse_the_cursor_before_start"),
            "raw_retention": ('attributes = {"record_kind": self.kind, "native_schema_version": version}',
                              'attributes = dict(record)', "test_literal_projection_and_public_read_contract"),
            "eof_as_timeout": ('"mcp_invalid_json" if self.buffer else "mcp_eof"', '"mcp_timeout"',
                               "test_eof_invalid_json_timeout_and_bounds_have_fixed_diagnostics"),
            "accept_future_version": ('version not in versions', 'False',
                                      "test_invalid_native_values_and_future_versions_are_rejected"),
            "leave_orphan_group": ('for sig in (signal.SIGTERM, signal.SIGKILL):',
                                   'for sig in (() if self.process.poll() is not None else '
                                   '(signal.SIGTERM, signal.SIGKILL)):',
                                   "test_shutdown_kills_descendants_after_the_parent_exits"),
        }
        for name, (before, after, target) in cases.items():
            with self.subTest(mutation=name):
                self.assertIn(before, original)
                module = importlib.util.module_from_spec(importlib.util.spec_from_loader("nodloop_live_mutant", None))
                exec(compile(original.replace(before, after, 1), "<nodloop-live-mutant>", "exec"), module.__dict__)
                result = unittest.TestResult()
                with mock.patch(__name__ + ".live", module):
                    NodloopLiveTest(target).run(result)
                self.assertFalse(result.wasSuccessful(), "mutation escaped: " + name)
                self.assertFalse(result.errors, "mutation crashed instead of failing an assertion: " + name)


class NativeProcessTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        if shutil.which("go") is None:
            raise unittest.SkipTest("Go is needed for the actual collector process test")
        cls.build = tempfile.TemporaryDirectory()
        cls.addClassCleanup(cls.build.cleanup)
        folder = Path(cls.build.name)
        source = folder / "native_group_test.go"
        source.write_text(COLLECTOR, encoding="utf-8")
        overlay = folder / "overlay.json"
        virtual = HERE.parents[1] / "internal/observe/command/native_group_test.go"
        overlay.write_text(json.dumps({"Replace": {str(virtual): str(source)}}), encoding="utf-8")
        cls.collector = folder / "collector"
        result = subprocess.run(["go", "test", "-c", "-overlay", str(overlay), "-o", str(cls.collector),
                                 "./internal/observe/command"], cwd=HERE.parents[1],
                                capture_output=True, text=True, timeout=60)
        if result.returncode:
            raise AssertionError(result.stdout + result.stderr)

    setUp = NodloopLiveTest.setUp
    page = NodloopLiveTest.page

    def stop_native(self) -> None:
        for name in ("native.pid", "child.pid"):
            path = self.folder / name
            if path.exists():
                try:
                    os.kill(int(path.read_text()), signal.SIGKILL)
                except ProcessLookupError:
                    pass

    tearDown = stop_native

    def arguments(self, managed: bool = True) -> list[str]:
        arguments = ["-B", str(READER), "--nodloop-program", str(self.program),
                     "--record-dir", str(self.folder), "--kind", "run", "--source-instance", "acme",
                     "--generation", "g1", "--stream", "runs"]
        return arguments + (["--managed-process-group"] if managed else [])

    def sentinel(self) -> subprocess.Popen:
        process = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"],
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
        self.addCleanup(process.wait, timeout=3)
        self.addCleanup(process.terminate)
        return process

    def native_ids(self) -> list[int]:
        paths = [self.folder / name for name in ("native.pid", "child.pid")]
        ready = self.folder / "native.ready"
        deadline = time.monotonic() + 3
        while not ready.exists() and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertTrue(ready.exists())
        self.assertEqual([path.exists() for path in paths], [True, True])
        return [int(path.read_text()) for path in paths]

    def assert_native_gone(self, identities: list[int]) -> None:
        remaining = list(identities)
        deadline = time.monotonic() + 3
        while remaining and time.monotonic() < deadline:
            for identity in list(remaining):
                try:
                    os.kill(identity, 0)
                except ProcessLookupError:
                    remaining.remove(identity)
            if remaining:
                time.sleep(0.01)
        self.assertEqual(remaining, [], "native processes escaped collector cleanup")

    def test_external_sigkill_leaves_no_native_child_and_preserves_sentinel(self) -> None:
        self.page(mode="cancel")
        sentinel = self.sentinel()
        reader = subprocess.Popen([sys.executable] + self.arguments(), stdin=subprocess.PIPE,
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
        try:
            reader.stdin.write(json.dumps(request()).encode())
            reader.stdin.close()
            identities = self.native_ids()
            group = int((self.folder / "native.pgid").read_text())
            os.killpg(reader.pid, signal.SIGKILL)
            reader.wait(timeout=3)
            self.assertEqual(reader.returncode, -signal.SIGKILL)
            self.assertEqual(group, reader.pid)
            self.assert_native_gone(identities)
            self.assertIsNone(sentinel.poll())
        finally:
            if reader.poll() is None:
                os.killpg(reader.pid, signal.SIGKILL)
            reader.wait(timeout=3)
            reader.stdout.close()
            reader.stderr.close()

    def test_managed_cli_requires_a_dedicated_reader_group_before_native_start(self) -> None:
        result = subprocess.run([sys.executable] + self.arguments(), input=json.dumps(request()),
                                capture_output=True, text=True, timeout=5)
        self.assertEqual((result.returncode, result.stdout, result.stderr), (1, "", "nodloop_live_failed\n"))
        self.assertFalse((self.folder / "native.pid").exists())

    def test_standalone_timeout_and_close_clean_owned_group_and_preserve_sentinel(self) -> None:
        sentinel = self.sentinel()
        for mode in ("cancel", "stubborn"):
            with self.subTest(mode=mode):
                self.stop_native()
                for name in ("native.pid", "native.pgid", "child.pid", "native.ready"):
                    (self.folder / name).unlink(missing_ok=True)
                self.page(mode=mode)
                self.reader.timeout = 0.4 if mode == "cancel" else 2
                actual = self.reader.read(request())
                self.assertEqual(actual["status"], "unavailable" if mode == "cancel" else "ok")
                if mode == "cancel":
                    self.assertIn("mcp_timeout", actual["coverage"]["limitations"])
                else:
                    self.assertEqual(actual["records"], [EXPECTED])
                identities = self.native_ids()
                self.assertEqual(int((self.folder / "native.pgid").read_text()), identities[0])
                self.assert_native_gone(identities)
                self.assertIsNone(sentinel.poll())

    def test_managed_close_reaps_only_direct_child_before_collector_cleanup(self) -> None:
        self.page(mode="stubborn")
        sentinel = self.sentinel()
        harness = ("import json,runpy,sys,time; "
                   "sys.argv=json.loads(sys.argv[1]); "
                   "scope=runpy.run_path(sys.argv[0]); "
                   "code=scope['main'](); sys.stdout.flush(); "
                   "time.sleep(30) if code==0 else sys.exit(code)")
        arguments = self.arguments()[1:]
        reader = subprocess.Popen([sys.executable, "-B", "-c", harness, json.dumps(arguments)],
                                  env=dict(os.environ, PYTHONPATH=str(HERE)), stdin=subprocess.PIPE,
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
        try:
            reader.stdin.write(json.dumps(request()).encode())
            reader.stdin.close()
            with selectors.DefaultSelector() as selector:
                selector.register(reader.stdout, selectors.EVENT_READ)
                self.assertTrue(selector.select(3), "managed close exceeded its bound")
                line = reader.stdout.readline()
            self.assertTrue(line, "reader exited before returning the page")
            self.assertEqual(json.loads(line)["records"], [EXPECTED])
            native, child = self.native_ids()
            self.assert_native_gone([native])
            os.kill(child, 0)
            self.assertEqual(os.getpgid(child), reader.pid)
            self.assertIsNone(reader.poll())
            self.assertIsNone(sentinel.poll())
        finally:
            if reader.poll() is None:
                os.killpg(reader.pid, signal.SIGKILL)
            reader.wait(timeout=3)
            reader.stdout.close()
            reader.stderr.close()

    def test_actual_collector_cleans_cancel_deadline_normal_close_and_orphan(self) -> None:
        sentinel = self.sentinel()
        cases = [("cancel", "cancel", "cancelled"), ("deadline", "cancel", "timeout"),
                 ("normal", "descendant", ""), ("normal", "orphan", "")]
        for index, (mode, native_mode, code) in enumerate(cases):
            with self.subTest(mode=mode, native_mode=native_mode):
                self.stop_native()
                self.page(mode=native_mode)
                for name in ("native.pid", "native.pgid", "child.pid", "native.ready"):
                    (self.folder / name).unlink(missing_ok=True)
                output = self.folder / ("result-" + str(index) + ".json")
                environment = dict(os.environ, ACME_PYTHON=sys.executable,
                                   ACME_READER_ARGS=json.dumps(self.arguments()), ACME_MODE=mode,
                                   ACME_READY=str(self.folder / "native.ready"), ACME_REQUEST=json.dumps(request()),
                                   ACME_RESULT=str(output))
                run = subprocess.run([str(self.collector), "-test.run=^TestNativeMCPGroup$"],
                                     env=environment, capture_output=True, text=True, timeout=10)
                self.assertEqual((run.returncode, run.stderr), (0, ""), run.stdout)
                actual = json.loads(output.read_text())
                self.assertEqual((actual["code"], actual["started"]), (code, True))
                if not code:
                    page = json.loads(actual["output"])
                    self.assertEqual(page["status"], "unavailable" if native_mode == "orphan" else "ok")
                    self.assertEqual(page["records"], [] if native_mode == "orphan" else [EXPECTED])
                self.assert_native_gone(self.native_ids())
                self.assertIsNone(sentinel.poll())

    def test_process_group_mutations_fail_literal_lifecycle_assertions(self) -> None:
        original = READER.read_text(encoding="utf-8")
        cases = {
            "escape_managed_group": ("start_new_session=not self.managed_process_group",
                                     "start_new_session=True",
                                     "test_external_sigkill_leaves_no_native_child_and_preserves_sentinel"),
            "skip_managed_close": ("self.stop_child()", "None",
                                   "test_managed_close_reaps_only_direct_child_before_collector_cleanup"),
            "skip_group_guard": ("args.managed_process_group and os.getpgrp() != os.getpid()", "False",
                                 "test_managed_cli_requires_a_dedicated_reader_group_before_native_start"),
        }
        for name, (before, after, target) in cases.items():
            with self.subTest(mutation=name), tempfile.TemporaryDirectory() as temporary:
                self.assertIn(before, original)
                folder = Path(temporary)
                mutant = folder / "nodloop_live.py"
                mutant.write_text(original.replace(before, after, 1), encoding="utf-8")
                shutil.copyfile(HERE / "adapter.py", folder / "adapter.py")
                shutil.copyfile(HERE / "stdio.py", folder / "stdio.py")
                result = unittest.TestResult()
                with mock.patch(__name__ + ".READER", mutant):
                    NativeProcessTest(target).run(result)
                self.assertFalse(result.wasSuccessful(), "mutation escaped: " + name)
                self.assertFalse(result.errors, "mutation crashed instead of failing an assertion: " + name)


if __name__ == "__main__":
    unittest.main()
