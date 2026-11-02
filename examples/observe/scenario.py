import argparse
import hashlib
import json
import os
from pathlib import Path
import queue
import subprocess
import sys
import tempfile
import threading
import time
from typing import Any

import stdio


HERE = Path(__file__).resolve().parent
SCHEDULER = '''import json
import os
from pathlib import Path
import plistlib
import shlex
import sys

folder = Path(__file__).parent
command = Path(sys.argv[0]).name
args = sys.argv[1:]
with (folder.parent / "scheduler-calls.jsonl").open("a", encoding="utf-8") as log:
    log.write(json.dumps({"command": command, "args": args}) + "\\n")
state_path = folder / "state.json"
state = json.loads(state_path.read_text()) if state_path.exists() else {}

def save():
    state_path.write_text(json.dumps(state), encoding="utf-8")

if command == "launchctl":
    if len(args) == 2 and args[0] == "print":
        loaded = state.get("launchd")
        if loaded is None or loaded["target"] != args[1]:
            raise SystemExit(113)
        config = loaded["config"]
        print(args[1] + " = {")
        print(" path = " + loaded["path"])
        print(" state = not running")
        print(" run interval = 300 seconds")
        print(" program = " + config["ProgramArguments"][0])
        print(" arguments = {")
        for value in config["ProgramArguments"]:
            print(" " + value)
        print(" }")
        print(" environment = {")
        for key, value in config["EnvironmentVariables"].items():
            print(" " + key + " => " + value)
        print(" }")
        print("}")
        raise SystemExit(0)
    if len(args) == 2 and args[0] == "print-disabled":
        print('disabled services = {\\n "dev.rindcut.tick" => enabled\\n}')
        raise SystemExit(0)
    if len(args) == 2 and args[0] == "enable":
        raise SystemExit(0)
    if len(args) == 3 and args[0] == "bootstrap":
        path = Path(args[2])
        if not path.resolve().is_relative_to(folder.parent.resolve()):
            raise SystemExit(2)
        config = plistlib.loads(path.read_bytes())
        state["launchd"] = {"target": args[1] + "/" + config["Label"], "path": str(path), "config": config}
        save()
        raise SystemExit(0)
    if len(args) == 2 and args[0] == "bootout":
        state.pop("launchd", None)
        save()
        raise SystemExit(0)
if command == "systemctl" and args and args[0] == "--user":
    args = args[1:]
    unit_dir = Path(os.environ["HOME"]) / ".config" / "systemd" / "user"
    if args == ["daemon-reload"]:
        loaded = state.setdefault("systemd", {})
        loaded["units"] = {path.name: path.read_text() for path in unit_dir.glob("rindcut-tick.*")}
        save()
        raise SystemExit(0)
    if len(args) == 3 and args[0] in ("enable", "disable") and args[1:] == ["--now", "rindcut-tick.timer"]:
        state.setdefault("systemd", {})["enabled"] = args[0] == "enable"
        save()
        raise SystemExit(0)
    if len(args) == 4 and args[0] == "show" and args[2].startswith("--property=") and args[3] == "--all":
        loaded = state.get("systemd", {})
        content = loaded.get("units", {}).get(args[1])
        values = {"LoadState": "not-found"}
        if content is not None:
            values = {"LoadState": "loaded", "FragmentPath": str(unit_dir / args[1]), "DropInPaths": ""}
            if args[1].endswith(".timer"):
                values.update(ActiveState="active" if loaded.get("enabled") else "inactive",
                              UnitFileState="enabled" if loaded.get("enabled") else "disabled",
                              Unit="rindcut-tick.service", Persistent="yes", TimersMonotonic="",
                              TimersCalendar="{ OnCalendar=*-*-* *:00/5:00 ; next_elapse=n/a }")
            else:
                fields = [line.split("=", 1) for line in content.splitlines() if "=" in line]
                environment = [shlex.split(value)[0].replace("%%", "%")
                               for key, value in fields if key == "Environment"]
                arguments = next(shlex.split(value) for key, value in fields if key == "ExecStart")
                arguments = [value.replace("%%", "%") for value in arguments]
                values["Environment"] = " ".join(json.dumps(value) if any(c.isspace() for c in value) else value
                                                 for value in environment)
                values["ExecStart"] = ("{ path=" + arguments[0] + " ; argv[]=" + " ".join(arguments)
                                       + " ; ignore_errors=no ; }")
        for key in args[2].removeprefix("--property=").split(","):
            print(key + "=" + values.get(key, ""))
        raise SystemExit(0)
sys.stderr.write("unsupported simulated scheduler request\\n")
raise SystemExit(2)
'''


def scheduler_environment(root: Path, home: Path) -> dict[str, str]:
    folder = root / "scheduler-bin"
    folder.mkdir()
    script = "#!" + sys.executable + "\n" + SCHEDULER
    for name in ("launchctl", "systemctl"):
        path = folder / name
        path.write_text(script, encoding="utf-8")
        path.chmod(0o700)
    return {"HOME": str(root), "RINDCUT_HOME": str(home), "PATH": str(folder) + os.pathsep + os.defpath,
            "PYTHONDONTWRITEBYTECODE": "1"}


def scheduler_calls(root: Path) -> list[dict[str, Any]]:
    path = root / "scheduler-calls.jsonl"
    return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines()] if path.exists() else []


class Client:
    def __init__(self, binary: Path, environment: dict[str, str]) -> None:
        self.process = subprocess.Popen([str(binary), "mcp"], env=environment, stdin=subprocess.PIPE,
                                        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
        self.reader = None
        try:
            self.messages = queue.Queue()
            self.sequence = 0
            self.failure = None
            self.reader = threading.Thread(target=self.read, daemon=True)
            self.reader.start()
            self.request("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                                        "clientInfo": {"name": "acme", "version": "1"}})
            self.send({"jsonrpc": "2.0", "method": "notifications/initialized"})
        except BaseException:
            self.close()
            raise

    def read(self) -> None:
        failure = RuntimeError("MCP stream closed")
        try:
            for line in self.process.stdout:
                try:
                    message = json.loads(line)
                except (ValueError, RecursionError):
                    failure = RuntimeError("MCP stream returned invalid JSON")
                    break
                if not isinstance(message, dict):
                    failure = RuntimeError("MCP stream returned non-object JSON")
                    break
                self.messages.put(message)
        except (OSError, ValueError, UnicodeError):
            failure = RuntimeError("MCP stream read failed")
        self.messages.put(failure)

    def send(self, value: dict[str, Any], *, deadline: float | None = None) -> None:
        if deadline is None:
            deadline = time.monotonic() + 20
        try:
            stdio.write(self.process.stdin, (json.dumps(value) + "\n").encode("utf-8"), deadline)
        except TimeoutError:
            raise TimeoutError("MCP response timed out") from None
        except (OSError, ValueError, UnicodeError):
            raise RuntimeError("MCP stream write failed") from None

    def request(self, method: str, parameters: dict[str, Any]) -> dict[str, Any]:
        if getattr(self, "failure", None) is not None:
            raise self.failure from None
        deadline = time.monotonic() + 20
        self.sequence += 1
        write_failure = None
        try:
            self.send({"jsonrpc": "2.0", "id": self.sequence, "method": method, "params": parameters}, deadline=deadline)
        except (RuntimeError, TimeoutError) as error:
            write_failure = error
        while True:
            try:
                if write_failure is not None:
                    result = self.messages.get_nowait()
                else:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        raise TimeoutError("MCP response timed out")
                    result = self.messages.get(timeout=remaining)
            except queue.Empty as error:
                if write_failure is not None:
                    self.failure = write_failure
                    raise write_failure from None
                raise TimeoutError("MCP response timed out") from error
            if isinstance(result, RuntimeError):
                self.failure = result
                raise result from None
            if result.get("id") != self.sequence:
                if write_failure is not None and time.monotonic() >= deadline:
                    self.failure = write_failure
                    raise write_failure from None
                continue
            if "error" in result:
                raise AssertionError(result["error"])
            return result["result"]

    def call(self, name: str, arguments: dict[str, Any]) -> dict[str, Any]:
        result = self.request("tools/call", {"name": name, "arguments": arguments})
        if result.get("isError"):
            raise AssertionError(result["content"])
        return result.get("structuredContent") or json.loads(result["content"][0]["text"])

    def close(self) -> None:
        try:
            self.process.stdin.close()
        except (OSError, ValueError):
            pass
        try:
            self.process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            try:
                self.process.kill()
            except ProcessLookupError:
                pass
            self.process.wait(timeout=5)
        reader = getattr(self, "reader", None)
        if reader is not None and reader.is_alive():
            reader.join(timeout=5)
        for stream in (self.process.stdout, self.process.stderr):
            if stream is not None:
                stream.close()


def write(path: Path, value: dict[str, Any]) -> None:
    path.write_text(json.dumps(value), encoding="utf-8")
    path.chmod(0o600)


def tree_hash(home: Path) -> dict[str, str]:
    return {str(path.relative_to(home)): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in home.rglob("*") if path.is_file()}


def records(client: Client, kind: str) -> list[dict[str, Any]]:
    query = {"source_id": "acme", "kind": kind, "limit": 100}
    result = []
    while True:
        page = client.call("observe_records", query)
        result.extend(page["records"])
        if not page["has_more"]:
            return result
        query["cursor"] = page["next_cursor"]


def wait_for(client: Client, expected: str) -> list[dict[str, Any]]:
    for _ in range(8):
        client.call("observe_sync", {})
        observations = records(client, "observation")
        if observations and observations[-1]["data"]["result"] == expected:
            return observations
        time.sleep(1.05)
    raise AssertionError("expected observation was not produced: " + expected)


def source_definition(journal: Path, destination: Path) -> dict[str, Any]:
    adapter = str(HERE / "adapter.py")
    return {
        "id": "acme", "enabled": True, "source_instance": "acme", "stream": "actions",
        "capability": {"kind": "event_stream", "types": [], "limitations": ["fixture_journal_only"]},
        "adapter_revision": "example-v1", "projection_profile": "metadata-v1",
        "reader": {"program": sys.executable, "args": [adapter, "reader", "--input", str(journal)],
                   "allowed_attributes": ["declared_claim", "declared_authority", "declared_verified"],
                   "fingerprint_files": [adapter]},
        "observers": [{"id": "actual_state", "types": ["acme.unregistered.terminal"],
                       "criterion_id": "actual_state", "criterion_revision": "C1",
                       "parameters": {"expected_state": "ready"}, "program": sys.executable,
                       "args": [adapter, "observer", "--input", str(destination)], "fingerprint_files": [adapter],
                       "mode": "watch", "actor_kind": "code", "interval_seconds": 1, "deadline_seconds": 60}],
    }


def run(binary: Path) -> dict[str, Any]:
    before_binary = hashlib.sha256(binary.read_bytes()).hexdigest()
    manifest = json.loads((HERE / "expected_manifest.json").read_text(encoding="utf-8"))
    with tempfile.TemporaryDirectory(prefix="acme-observe-") as folder:
        root = Path(folder)
        home, journal, destination = root / "home", root / "journal.json", root / "destination.json"
        write(journal, {"generation": "g1", "records": [{"id": "e1", "type": "acme.unregistered.terminal",
              "action_id": "A1", "claim": "success", "authority": "person", "verified": True}]})
        write(destination, {"effects": {}})
        environment = scheduler_environment(root, home)
        client = Client(binary, environment)
        try:
            request = {"definition": source_definition(journal, destination), "expected_revision": 0}
            preview = client.call("source_preview", request)
            request.update(preview_digest=preview["preview_digest"], confirmed=True)
            client.call("source_save", request)
            pending = wait_for(client, "pending")
            write(destination, {"effects": {"A1": {"state": "ready"}}})
            confirmed = wait_for(client, "confirmed")
            previous = confirmed[-1]["data"]["observation_id"]
            write(destination, {"effects": {"A1": {"state": "wrong"}}, "correct_previous": True})
            observations = wait_for(client, "refuted")
            events = records(client, "event")
            actual = [{key: record["data"][key] for key in manifest["events"][0]} for record in events]
            assert actual == manifest["events"], "event manifest mismatch"
            transitions = []
            series = set()
            observation_ids = set()
            for record in observations:
                value = record["data"]["result"]
                transitions.append(value)
                series.add(record["data"]["series_id"])
                observation_ids.add(record["data"]["observation_id"])
                assert record["id"] == record["data"]["observation_id"]
                assert record["data"]["target_ref"]["capture_id"] == events[0]["data"]["capture_id"]
                assert record["data"]["actor_kind"] == manifest["actor_kind"]
                assert "authority" not in record["data"] and "verified" not in record["data"]
            assert transitions == manifest["observation_results"], "observation manifest mismatch"
            assert len(series) == 1 and len(observation_ids) == len(observations)
            assert pending[-1]["data"]["result"] == "pending"
            last = observations[-1]["data"]
            assert last["supersedes"]["observation_id"] == previous
            assert last["target_ref"] == confirmed[-1]["data"]["target_ref"]
            before_queries = tree_hash(home)
            public = client.call("observe_records", {"source_id": "acme", "kind": "event", "limit": 100})
            result = subprocess.run([str(binary), "observe", "records", "--source-id", "acme", "--kind", "event"],
                                    env=environment, capture_output=True, text=True, timeout=10, check=True)
            rows = [json.loads(line) for line in result.stdout.splitlines()]
            footer = rows.pop()
            assert rows == public["records"] and footer == {
                "kind": "page", "next_cursor": public["next_cursor"], "has_more": public["has_more"],
                "store_present": public["store_present"],
            }, "JSONL framing mismatch"
            client.call("observe_status", {})
            assert tree_hash(home) == before_queries, "read-only query changed files"
            assert str(journal) not in json.dumps(public) and str(HERE) not in json.dumps(public)
            scheduler_count = len(scheduler_calls(root))
        finally:
            client.close()
    assert hashlib.sha256(binary.read_bytes()).hexdigest() == before_binary
    return {"binary_sha256": before_binary, "events_match_manifest": True, "observation_results": transitions,
            "explicit_correction_preserved": True, "JSONL_matches_MCP": True, "queries_are_read_only": True,
            "authority_not_promoted": True, "live_provider_calls": 0,
            "os_scheduler_isolated": True, "simulated_scheduler_commands": scheduler_count}


def main() -> None:
    parser = argparse.ArgumentParser(description="Run the isolated acme scenario against one unchanged rindcut binary")
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    result = run(args.binary.resolve())
    text = json.dumps(result, indent=2) + "\n"
    if args.output:
        args.output.write_text(text, encoding="utf-8")
    print(text, end="")


if __name__ == "__main__":
    main()
