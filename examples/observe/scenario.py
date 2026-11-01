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


HERE = Path(__file__).resolve().parent


class Client:
    def __init__(self, binary: Path, environment: dict[str, str]) -> None:
        self.process = subprocess.Popen([str(binary), "mcp"], env=environment, stdin=subprocess.PIPE,
                                        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
        self.messages = queue.Queue()
        self.sequence = 0
        threading.Thread(target=self.read, daemon=True).start()
        self.request("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                                    "clientInfo": {"name": "acme", "version": "1"}})
        self.send({"jsonrpc": "2.0", "method": "notifications/initialized"})

    def read(self) -> None:
        for line in self.process.stdout:
            self.messages.put(json.loads(line))

    def send(self, value: dict[str, Any]) -> None:
        self.process.stdin.write(json.dumps(value) + "\n")
        self.process.stdin.flush()

    def request(self, method: str, parameters: dict[str, Any]) -> dict[str, Any]:
        self.sequence += 1
        self.send({"jsonrpc": "2.0", "id": self.sequence, "method": method, "params": parameters})
        while True:
            result = self.messages.get(timeout=20)
            if result.get("id") != self.sequence:
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
        self.process.stdin.close()
        try:
            self.process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait(timeout=5)


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
        environment = {"HOME": str(root), "RINDCUT_HOME": str(home), "PATH": os.defpath}
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
        finally:
            client.close()
    assert hashlib.sha256(binary.read_bytes()).hexdigest() == before_binary
    return {"binary_sha256": before_binary, "events_match_manifest": True, "observation_results": transitions,
            "explicit_correction_preserved": True, "JSONL_matches_MCP": True, "queries_are_read_only": True,
            "authority_not_promoted": True, "live_provider_calls": 0}


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
