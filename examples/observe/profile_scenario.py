import argparse
import copy
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import time
from typing import Any

from scenario import Client, scheduler_calls, scheduler_environment, tree_hash, write
import profile_matrix
import usage


HERE = Path(__file__).resolve().parent
CALLER = '''import json
from pathlib import Path
import subprocess
import sys
wire = sys.stdin.buffer.read((1 << 20) + 1)
request = json.loads(wire)
entry = {"mode": sys.argv[3], "source_id": request["source_id"]}
if "event" in request:
    entry.update(event_id=request["event"]["id"], event_type=request["event"]["type"])
with Path(sys.argv[1]).open("a", encoding="utf-8") as log:
    log.write(json.dumps(entry) + "\\n")
result = subprocess.run([sys.executable, *sys.argv[2:]], input=wire, capture_output=True, check=False)
sys.stdout.buffer.write(result.stdout)
sys.stderr.buffer.write(result.stderr)
raise SystemExit(result.returncode)
'''


def fixture(name: str) -> dict[str, Any]:
    return json.loads((HERE / name).read_text(encoding="utf-8"))


class Harness:
    def __init__(self, binary: Path, folder: Path) -> None:
        self.binary = binary
        self.folder = folder
        self.home = folder / "home"
        self.calls = folder / "calls.jsonl"
        self.wrapper = folder / "caller.py"
        self.wrapper.write_text(CALLER, encoding="utf-8")
        self.environment = scheduler_environment(folder, self.home)
        self.client = Client(binary, self.environment)

    def cli(self, *arguments: str, as_json: bool = True) -> Any:
        command = [str(self.binary), *(["--json"] if as_json else []), *arguments]
        result = subprocess.run(command, env=self.environment, capture_output=True, text=True, timeout=20, check=True)
        assert not result.stderr, result.stderr
        return json.loads(result.stdout) if as_json else result.stdout

    def calls_so_far(self) -> list[dict[str, Any]]:
        return [json.loads(line) for line in self.calls.read_text(encoding="utf-8").splitlines()] if self.calls.exists() else []

    def definition(self, name: str) -> dict[str, Any]:
        definition = fixture(name + "_definition.json")
        definition["reader"]["limit"] = 1
        for command in [definition["reader"], *definition["observers"]]:
            original = command["args"]
            profile = HERE / Path(original[0]).name
            source = self.folder / Path(original[-1]).name
            command["program"] = sys.executable
            command["args"] = [str(self.wrapper), str(self.calls), str(profile), original[1], "--input", str(source)]
            command["fingerprint_files"] = [str(HERE / Path(path).name) for path in command["fingerprint_files"]]
            command["fingerprint_files"].append(str(self.wrapper))
        return definition

    def save(self, definition: dict[str, Any], revision: int, recovery: str | None = None,
             generation: str | None = None) -> dict[str, Any]:
        request = {"definition": definition, "expected_revision": revision}
        before = self.calls_so_far()
        preview = self.client.call("source_preview", request)
        assert self.calls_so_far() == before, "preview invoked an adapter"
        if recovery:
            request["recovery"] = {"mode": recovery, "checkpoint_token": preview["checkpoint_token"]}
            if recovery == "reset":
                request["recovery"].update(collection_identity=preview["collection_identity"], generation=generation)
            preview = self.client.call("source_preview", request)
            assert self.calls_so_far() == before, "recovery preview invoked an adapter"
        request.update(preview_digest=preview["preview_digest"], confirmed=True)
        return self.client.call("source_save", request)

    def records(self, source: str, kind: str) -> list[dict[str, Any]]:
        query = {"source_id": source, "kind": kind, "limit": 1}
        rows = []
        for _ in range(1000):
            page = self.client.call("observe_records", query)
            rows.extend(page["records"])
            if not page["has_more"]:
                return rows
            assert page["next_cursor"], "missing continuation"
            query["cursor"] = page["next_cursor"]
        raise AssertionError("query did not terminate")

    def wait_for(self, source: str, expected: str) -> list[dict[str, Any]]:
        for _ in range(8):
            self.client.call("observe_sync", {})
            rows = self.records(source, "observation")
            if rows and rows[-1]["data"]["result"] == expected:
                return rows
            time.sleep(1.05)
        raise AssertionError("expected observation missing: " + source + " " + expected)

    def readonly_queries(self, sources: list[str]) -> int:
        before, calls = tree_hash(self.home), self.calls_so_far()
        count = 0
        for source in sources:
            for kind in ("event", "observation"):
                query = {"source_id": source, "kind": kind, "limit": 1}
                all_rows = []
                seen = set()
                for _ in range(1000):
                    public = self.client.call("observe_records", query)
                    arguments = ["observe", "records", "--source-id", source, "--kind", kind, "--limit", "1"]
                    if "cursor" in query:
                        arguments.extend(["--cursor", query["cursor"]])
                    wire = self.cli(*arguments, as_json=False)
                    rows = [json.loads(line) for line in wire.splitlines()]
                    footer = rows.pop()
                    assert rows == public["records"], "CLI rows differ from MCP"
                    assert footer == {"kind": "page", "next_cursor": public["next_cursor"],
                                      "has_more": public["has_more"], "store_present": public["store_present"]}
                    assert self.cli(*arguments) == public, "CLI JSON differs from MCP"
                    assert str(self.folder) not in wire and str(HERE) not in wire, "query disclosed adapter paths"
                    all_rows.extend(rows)
                    count += 1
                    if not public["has_more"]:
                        break
                    cursor = public["next_cursor"]
                    assert isinstance(cursor, str) and cursor and cursor not in seen, "query cursor did not advance"
                    seen.add(cursor)
                    query["cursor"] = cursor
                else:
                    raise AssertionError("query did not terminate")
                assert all_rows == self.records(source, kind), "pagination changed record order"
        self.client.call("observe_status", {})
        self.cli("observe", "status")
        assert tree_hash(self.home) == before, "read-only query changed home bytes"
        assert self.calls_so_far() == calls, "read-only query invoked reader or observer"
        return count


def run(binary: Path) -> dict[str, Any]:
    binary_hash = hashlib.sha256(binary.read_bytes()).hexdigest()
    expected_usage = fixture("usage_expected_manifest.json")
    expected_a2a = fixture("a2a_expected_manifest.json")
    with tempfile.TemporaryDirectory(prefix="acme-profiles-") as folder:
        harness = None
        try:
            harness = Harness(binary, Path(folder))
            a2a_journal = fixture("a2a_snapshots.json")
            a2a_destination = fixture("a2a_destination.json")
            pending = copy.deepcopy(a2a_destination)
            pending["tasks"][expected_a2a["task_ref"]].update(complete=False, effects=[])
            usage_journal = fixture("usage_journal.json")
            partial = copy.deepcopy(usage_journal)
            partial["snapshots"] = partial["snapshots"][:1]
            write(harness.folder / "a2a_snapshots.json", a2a_journal)
            write(harness.folder / "a2a_destination.json", pending)
            write(harness.folder / "usage_journal.json", partial)
            write(harness.folder / "usage_resolver.json", fixture("usage_resolver.json"))
            a2a_definition = harness.definition("a2a")
            usage_definition = harness.definition("usage")
            harness.save(a2a_definition, 0)
            harness.save(usage_definition, 0)
            assert harness.calls_so_far() == [], "registration executed source commands"
            harness.cli("observe", "sync")
            a2a_pending = harness.wait_for("acme-a2a", "pending")
            assert len(harness.records("acme-a2a", "event")) == 1
            assert [row["data"]["id"] for row in harness.records("usage", "event")] == ["S-partial"]
            assert harness.records("usage", "observation") == []
            assert not [call for call in harness.calls_so_far() if call["mode"] == "observer" and call["source_id"] == "usage"]
            harness.client.close()
            harness.client = Client(binary, harness.environment)
            assert harness.records("acme-a2a", "observation") == a2a_pending, "restart changed history"
            write(harness.folder / "usage_journal.json", usage_journal)
            write(harness.folder / "a2a_destination.json", a2a_destination)
            harness.wait_for("usage", expected_usage["boundary_result"])
            a2a_confirmed = harness.wait_for("acme-a2a", "confirmed")
            usage_events = harness.records("usage", "event")
            assert [row["data"]["id"] for row in usage_events] == expected_usage["event_ids"]
            assert [row["data"]["type"] for row in usage_events] == expected_usage["event_types"]
            selected = usage.select_snapshot([row["data"] for row in usage_events])
            assert selected["id"] == expected_usage["selected_snapshot"]
            assert {key: selected["attributes"][key] for key in expected_usage["normalized_run"]} == expected_usage["normalized_run"]
            assert selected["id"] != usage_events[0]["data"]["id"], "partial was overwritten"
            usage_observations = harness.records("usage", "observation")
            assert len(usage_observations) == 1, "partial created an observation series"
            assert usage_observations[0]["data"]["target_ref"]["capture_id"] == usage_events[1]["data"]["capture_id"]
            assert a2a_confirmed[-1]["data"]["target_ref"] == a2a_pending[0]["data"]["target_ref"]
            before_events = {source: harness.records(source, "event") for source in ("usage", "acme-a2a")}
            harness.cli("observe", "sync")
            assert all(harness.records(source, "event") == rows for source, rows in before_events.items()), "same page was recaptured"
            previous = harness.records("acme-a2a", "observation")[-1]["data"]["observation_id"]
            corrected = copy.deepcopy(a2a_destination)
            corrected["artifacts"][expected_a2a["artifact_ref"]]["content_base64"] = "d3JvbmcK"
            corrected["correct_previous"] = True
            write(harness.folder / "a2a_destination.json", corrected)
            a2a_refuted = harness.wait_for("acme-a2a", "refuted")
            assert a2a_refuted[-1]["data"]["supersedes"]["observation_id"] == previous
            corrected["correct_previous"] = False
            write(harness.folder / "a2a_destination.json", corrected)
            changed_observer = copy.deepcopy(usage_definition)
            changed_observer["observers"][0]["criterion_revision"] = "C2"
            changed_observer["observers"][0]["parameters"]["expected_budget_revision"] = "2"
            harness.save(changed_observer, 1)
            harness.client.call("observe_sync", {})
            assert harness.records("usage", "observation") == usage_observations, "definition edit silently rejudged history"
            harness.save(changed_observer, 2, recovery="backfill")
            usage_rejudged = harness.wait_for("usage", "unknown")
            assert usage_rejudged[-1]["data"]["criterion_revision"] == "C2"
            assert usage_rejudged[-1]["data"]["capture_source_revision"] == 1
            assert usage_rejudged[-1]["data"]["source_revision"] == 3
            assert usage_rejudged[-1]["data"]["series_id"] != usage_observations[0]["data"]["series_id"]
            assert harness.records("usage", "event") == usage_events
            assert not [call for call in harness.calls_so_far() if call.get("event_type") == "acme.run_usage.partial"]
            conflict = copy.deepcopy(a2a_journal["samples"][0])
            conflict["task"]["status"]["state"] = "TASK_STATE_WORKING"
            a2a_journal["samples"].extend([copy.deepcopy(a2a_journal["samples"][0]), conflict])
            write(harness.folder / "a2a_snapshots.json", a2a_journal)
            harness.client.call("observe_sync", {})
            conflicted = harness.records("acme-a2a", "event")
            assert len(conflicted) == 2 and len({row["data"]["id"] for row in conflicted}) == 1
            assert len({row["data"]["normalized_digest"] for row in conflicted}) == 2
            status = harness.client.call("observe_status", {})
            assert next(row for row in status["sources"] if row["source"]["id"] == "acme-a2a")["conflicts"] == 1
            a2a_journal["generation"] = "g2"
            a2a_journal["samples"] = a2a_journal["samples"][:1]
            write(harness.folder / "a2a_snapshots.json", a2a_journal)
            harness.client.call("observe_sync", {})
            status = harness.client.call("observe_status", {})
            blocked = next(row for row in status["sources"] if row["source"]["id"] == "acme-a2a")
            assert blocked["gap"] and blocked["blocked"], "generation change silently reset the cursor"
            assert harness.records("acme-a2a", "event") == conflicted
            harness.save(a2a_definition, 1, recovery="reset", generation="g2")
            harness.client.call("observe_sync", {})
            recovered = harness.records("acme-a2a", "event")
            assert len(recovered) == 3 and recovered[-1]["data"]["generation"] == "g2"
            pages = harness.readonly_queries(["acme-a2a", "usage"])
            for source in ("acme-a2a", "usage"):
                for row in harness.records(source, "observation"):
                    assert row["data"]["actor_kind"] == "code"
                    assert "authority" not in row["data"] and "verified" not in row["data"]
            calls = harness.calls_so_far()
            partial_calls = sum(call.get("event_type") == "acme.run_usage.partial" for call in calls)
            scheduler_count = len(scheduler_calls(harness.folder))
        finally:
            if harness is not None:
                harness.client.close()
    assert hashlib.sha256(binary.read_bytes()).hexdigest() == binary_hash, "binary changed during acceptance"
    matrix = profile_matrix.run(binary, Harness)
    assert matrix["binary_sha256"] == binary_hash, "binary changed between lifecycle and matrix cases"
    return {"binary_sha256": binary_hash, "profiles": ["a2a.task.snapshot.v1", "acme.run_usage.snapshot.v1"],
            "independent_manifest_matches": True, "partial_observer_calls": partial_calls,
            "late_final_is_new_capture": True, "restart_and_replay_preserve_captures": True,
            "explicit_backfill_uses_new_criterion_series": True, "identity_conflict_preserved": True,
            "generation_change_requires_explicit_reset": True, "explicit_correction_preserved": True,
            "jsonl_pages_match_mcp": pages, "readonly_query_adapter_calls": 0,
            "readonly_home_hash_unchanged": True, "authority_not_promoted": True, "live_provider_calls": 0,
            "os_scheduler_isolated": True, "simulated_scheduler_commands": scheduler_count,
            "acceptance_matrix": matrix}


def main() -> None:
    parser = argparse.ArgumentParser(description="Run both offline profiles through one rindcut binary and public tools")
    parser.add_argument("--binary", required=True, type=Path)
    args = parser.parse_args()
    print(json.dumps(run(args.binary.resolve()), indent=2))


if __name__ == "__main__":
    main()
