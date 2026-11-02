import copy
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from typing import Any

import a2a


HERE = Path(__file__).resolve().parent
AT = "2026-01-01T00:00:00Z"


def fixture(name: str) -> dict[str, Any]:
    return json.loads((HERE / ("a2a_" + name + ".json")).read_text(encoding="utf-8"))


def source_request() -> dict[str, Any]:
    return {"protocol": "rindcut.source/v1", "operation": "read", "source_id": "acme-a2a", "source_revision": 1,
            "source_instance": "acme-a2a", "stream": "tasks", "generation": None, "cursor": None, "limit": 100}


def observer_request(event: dict[str, Any]) -> dict[str, Any]:
    return {"protocol": "rindcut.observer/v1", "source_id": "acme-a2a", "source_revision": 1,
            "observer_id": "effect", "observer_revision": 1, "criterion_id": "a2a_effect", "criterion_revision": "C1",
            "series_id": "S1", "target_ref": {"store_id": "store", "capture_id": "C1"}, "event": event,
            "parameters": {"expected_scope_ref": fixture("expected_manifest")["scope_ref"], "expected_state": "ready",
                           "expected_window_start": AT, "expected_window_end": "2026-01-01T00:10:00Z",
                           "require_artifact": True, "byte_equality": True}, "now": "2026-01-01T00:15:00Z"}


class A2ATest(unittest.TestCase):
    def setUp(self) -> None:
        self.journal = fixture("snapshots")
        self.destination = fixture("destination")
        self.expected = fixture("expected_manifest")

    def event(self, journal: dict[str, Any] | None = None) -> dict[str, Any]:
        return a2a.read_source(source_request(), self.journal if journal is None else journal)["records"][0]

    def invoke(self, request: Any, source: dict[str, Any], mode: str, raw: bool = False) -> subprocess.CompletedProcess:
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "input.json"
            path.write_text(json.dumps(source), encoding="utf-8")
            return subprocess.run([sys.executable, str(HERE / "a2a.py"), mode, "--input", str(path)],
                                  input=request if raw else json.dumps(request), capture_output=True,
                                  text=True, timeout=5, check=False)

    def test_source_projection_matches_independent_manifest(self) -> None:
        result = self.invoke(source_request(), self.journal, "reader")
        self.assertEqual(result.returncode, 0, result.stderr)
        page = json.loads(result.stdout)
        event = page["records"][0]
        self.assertEqual(event["id"], "s1")
        self.assertEqual(event["action_id"], self.expected["task_ref"])
        self.assertEqual(event["attributes"], {"scope_ref": self.expected["scope_ref"], "wire_version": "1.0",
                         "context_ref": self.expected["context_ref"], "task_state": "TASK_STATE_COMPLETED",
                         "artifact_state": "present", "chunks_complete": True})
        self.assertEqual(event["occurred_at"], "2026-01-01T00:00:00.000000000Z")
        self.assertEqual(event["evidence"][0], {"kind": "artifact_version", "ref": {
            "namespace": "a2a.artifact", "id": self.expected["artifact_ref"]}, "digest": self.expected["artifact_digest"]})
        self.assertEqual(event["evidence"][1]["ref"]["id"], self.expected["snapshot_ref"])
        self.assertEqual(event["evidence"][1]["digest"], self.expected["snapshot_digest"])
        self.assertEqual(page["capability"]["kind"], "snapshot")
        self.assertIn("no_missing_chunk_reconstruction", page["coverage"]["limitations"])
        for private in ("private text", "private prompt", "private token", "https://", '"history"', '"metadata"'):
            self.assertNotIn(private, result.stdout)
        self.assertNotIn("attempt_id", event)

    def test_all_six_acceptance_families(self) -> None:
        for name, expected in self.expected["cases"].items():
            with self.subTest(case=name):
                journal, destination = copy.deepcopy(self.journal), copy.deepcopy(self.destination)
                task = destination["tasks"][self.expected["task_ref"]]
                if name == "coverage_window_mismatch":
                    task["window_end"] = "2026-01-01T00:09:00Z"
                if name == "required_artifact_missing":
                    del journal["samples"][0]["artifact"]
                    del journal["samples"][0]["task"]["artifacts"]
                    task["artifact_state"] = "absent"
                if name == "delayed":
                    task.update(complete=False, effects=[])
                if name == "chunk_gap":
                    journal["samples"][0]["chunks_complete"] = False
                if name == "unreadable":
                    destination["readable"] = False
                if name.startswith("cancel_"):
                    journal["samples"][0]["task"]["status"]["state"] = "TASK_STATE_CANCELED"
                    if name == "cancel_unknown":
                        del task["effects"][0]["occurred_at"]
                if name == "duplicate_effect":
                    task["effects"].append(dict(task["effects"][0], id="effect-2"))
                if name == "digest_mismatch":
                    destination["artifacts"][self.expected["artifact_ref"]]["content_base64"] = "d3JvbmcK"
                if name == "version_unavailable":
                    destination["artifacts"] = {}
                request = observer_request(self.event(journal))
                if name.startswith("cancel_"):
                    request["parameters"]["cancel_cutoff"] = ("2026-01-01T00:02:00Z" if name == "cancel_before"
                                                               else "2026-01-01T00:00:30Z")
                if name == "duplicate_effect":
                    request["parameters"]["idempotency_scope"] = "order-1"
                result = self.invoke(request, destination, "observer")
                self.assertEqual(result.returncode, 0, result.stderr)
                value = json.loads(result.stdout)
                self.assertEqual([value["result"], value["reason_code"]], expected)
                self.assertEqual(value["target_ref"], request["target_ref"])
                self.assertNotIn("supersedes", value)
                self.assertNotIn("authority", value)

    def test_delayed_receipt_and_deadline_do_not_rewrite_snapshot(self) -> None:
        request = observer_request(self.event())
        task = self.destination["tasks"][self.expected["task_ref"]]
        task.update(complete=False, effects=[])
        before = copy.deepcopy(request)
        request["now"] = "2027-01-01T00:00:00Z"
        self.assertEqual(a2a.observe(request, self.destination)["result"], "pending")
        self.assertEqual(request["event"], before["event"])
        self.assertEqual(a2a.observe(request, fixture("destination"))["result"], "confirmed")
        closed = fixture("destination")
        closed["tasks"][self.expected["task_ref"]]["effects"] = []
        self.assertEqual(a2a.observe(request, closed)["reason_code"], "required_effect_missing")

    def test_replay_new_sample_refinement_generation_and_backfill(self) -> None:
        request = source_request()
        request["limit"] = 1
        first = a2a.read_source(request, self.journal)
        self.assertEqual(a2a.read_source(request, self.journal), first)
        request.update(generation="g1", cursor=first["next_cursor"])
        self.assertEqual(a2a.read_source(request, self.journal)["records"], [])
        second = copy.deepcopy(self.journal["samples"][0])
        second["sample_id"] = "s2"
        second["task"]["id"] = "T2"
        second["links"] = [{"relation": "caused_by", "sample_id": "s1"}]
        self.journal["samples"].append(second)
        page = a2a.read_source(request, self.journal)
        self.assertEqual(page["records"][0]["id"], "s2")
        self.assertNotEqual(page["records"][0]["action_id"], first["records"][0]["action_id"])
        self.assertEqual(page["records"][0]["attributes"]["context_ref"], self.expected["context_ref"])
        self.assertEqual(page["records"][0]["links"][0]["target"]["id"], "s1")
        changed = copy.deepcopy(self.journal)
        changed["samples"][0]["task"]["status"]["state"] = "TASK_STATE_WORKING"
        self.assertEqual(a2a.read_source(request, changed)["status"], "cursor_invalid")
        changed = copy.deepcopy(self.journal)
        changed["generation"] = "g2"
        self.assertEqual(a2a.read_source(request, changed)["status"], "cursor_invalid")
        self.assertEqual(len(a2a.read_source(source_request(), self.journal)["records"]), 2)

    def test_no_protocol_idempotency_or_approval_inference(self) -> None:
        task = self.destination["tasks"][self.expected["task_ref"]]
        task["effects"].append(dict(task["effects"][0], id="effect-2"))
        request = observer_request(self.event())
        self.assertEqual(a2a.observe(request, self.destination)["result"], "confirmed")
        request["parameters"]["idempotency_scope"] = "another-order"
        self.assertEqual(a2a.observe(request, self.destination)["result"], "unknown")
        request["parameters"]["idempotency_scope"] = "order-1"
        self.assertEqual(a2a.observe(request, self.destination)["result"], "refuted")

    def test_binding_missing_version_and_optional_artifacts(self) -> None:
        request = observer_request(self.event())
        request["parameters"]["expected_scope_ref"] = "0" * 64
        self.assertEqual(a2a.observe(request, self.destination)["result"], "unknown")

        request = observer_request(self.event())
        request["event"]["evidence"][0]["digest"] = "sha256:" + "0" * 64
        self.assertEqual(a2a.observe(request, self.destination)["reason_code"], "snapshot_binding_mismatch")
        del self.journal["samples"][0]["artifact"]
        request = observer_request(self.event())
        self.assertEqual(a2a.observe(request, self.destination)["reason_code"], "artifact_version_missing")
        request["parameters"].update(require_artifact=False, byte_equality=False)
        self.assertEqual(a2a.observe(request, self.destination)["result"], "confirmed")
        del self.journal["samples"][0]["task"]["artifacts"]
        self.destination["tasks"][self.expected["task_ref"]]["artifact_state"] = "present"
        request = observer_request(self.event())
        self.assertEqual(a2a.observe(request, self.destination)["result"], "unknown")

    def test_stale_version_bytes_do_not_prove_task_attachment(self) -> None:
        cases = [("present", "confirmed", "criterion_satisfied"),
                 ("absent", "refuted", "required_artifact_missing"),
                 ("unknown", "unknown", "artifact_presence_unknown")]
        for state, expected, reason in cases:
            with self.subTest(state=state):
                destination = copy.deepcopy(self.destination)
                destination["tasks"][self.expected["task_ref"]]["artifact_state"] = state
                response = a2a.observe(observer_request(self.event()), destination)
                self.assertEqual((expected, reason), (response["result"], response["reason_code"]))

    def test_explicit_correction_is_distinct_from_new_snapshot(self) -> None:
        request = observer_request(self.event())
        request["previous_observation_id"] = "O1"
        self.assertNotIn("supersedes", a2a.observe(request, self.destination))
        self.destination["correct_previous"] = True
        self.assertEqual(a2a.observe(request, self.destination)["supersedes"],
                         {"store_id": "store", "observation_id": "O1"})
        del request["previous_observation_id"]
        with self.assertRaises(ValueError):
            a2a.observe(request, self.destination)

    def test_cancel_nanosecond_boundary_and_unknown_order(self) -> None:
        self.journal["samples"][0]["task"]["status"]["state"] = "TASK_STATE_CANCELED"
        request = observer_request(self.event())
        request["parameters"]["cancel_cutoff"] = "2026-01-01T00:01:00.000000002Z"
        effect = self.destination["tasks"][self.expected["task_ref"]]["effects"][0]
        effect["occurred_at"] = "2026-01-01T00:01:00.000000001Z"
        self.assertEqual(a2a.observe(request, self.destination)["result"], "confirmed")
        effect["occurred_at"] = "2026-01-01T00:01:00.000000002Z"
        self.assertEqual(a2a.observe(request, self.destination)["result"], "refuted")
        del request["parameters"]["cancel_cutoff"]
        self.assertEqual(a2a.observe(request, self.destination)["result"], "unknown")

    def test_future_coverage_cannot_prove_absence(self) -> None:
        request = observer_request(self.event())
        request["now"] = AT
        self.destination["tasks"][self.expected["task_ref"]]["effects"] = []
        self.assertEqual(a2a.observe(request, self.destination)["reason_code"], "coverage_not_closed")

    def test_timestamp_projection_survives_core_canonicalization(self) -> None:
        self.journal["samples"][0]["sampled_at"] = "2026-01-01T01:00:00.000000001+01:00"
        event = self.event()
        self.assertEqual(event["occurred_at"], "2026-01-01T00:00:00.000000001Z")
        self.assertEqual(a2a.observe(observer_request(event), self.destination)["result"], "confirmed")

    def test_empty_optional_context_has_no_invented_identity(self) -> None:
        self.journal["samples"][0]["task"]["contextId"] = ""
        self.assertNotIn("context_ref", self.event()["attributes"])
        self.assertEqual(a2a.observe(observer_request(self.event()), self.destination)["result"], "confirmed")

    def test_fixed_window_cannot_be_omitted_or_reversed(self) -> None:
        request = observer_request(self.event())
        del request["parameters"]["expected_window_end"]
        with self.assertRaises(ValueError):
            a2a.observe(request, self.destination)
        request["parameters"]["expected_window_end"] = "2025-01-01T00:00:00Z"
        with self.assertRaises(ValueError):
            a2a.observe(request, self.destination)

    def test_invalid_and_sensitive_inputs_fail_without_output(self) -> None:
        wrong_version = copy.deepcopy(self.journal)
        wrong_version["wire_version"] = "0.3"
        wrong_state = copy.deepcopy(self.journal)
        wrong_state["samples"][0]["task"]["status"]["state"] = "TASK_STATE_CANCELLED"
        wrong_attribute = copy.deepcopy(self.journal)
        wrong_attribute["samples"][0]["authorization"] = "private token"
        wrong_scope = copy.deepcopy(self.journal)
        wrong_scope["scope"]["caller"] = "https://example.com/private"
        for journal in (wrong_version, wrong_state, wrong_attribute, wrong_scope):
            with self.subTest(journal=journal):
                result = self.invoke(source_request(), journal, "reader")
                self.assertEqual((result.returncode, result.stdout, result.stderr), (1, "", "a2a_adapter_failed\n"))
        duplicate = json.dumps(source_request())[:-1] + ',"source_id":"private token"}'
        result = self.invoke(duplicate, self.journal, "reader", raw=True)
        self.assertEqual((result.returncode, result.stdout, result.stderr), (1, "", "a2a_adapter_failed\n"))
        request = observer_request(self.event())
        request["parameters"]["program"] = "private token"
        result = self.invoke(request, self.destination, "observer")
        self.assertEqual((result.returncode, result.stdout, result.stderr), (1, "", "a2a_adapter_failed\n"))

    def test_mutation_guard_rejects_broken_digest_judgment(self) -> None:
        source = (HERE / "a2a.py").read_text(encoding="utf-8")
        broken = source.replace('if parameters["byte_equality"] and observed != expected["digest"]:',
                                'if False:')
        self.assertNotEqual(source, broken)
        module = importlib.util.module_from_spec(importlib.util.spec_from_loader("a2a_mutant", loader=None))
        exec(compile(broken, "<a2a-mutant>", "exec"), module.__dict__)
        self.destination["artifacts"][self.expected["artifact_ref"]]["content_base64"] = "d3JvbmcK"
        expected = self.expected["cases"]["digest_mismatch"][0]
        request = observer_request(self.event())
        self.assertEqual(a2a.observe(request, self.destination)["result"], expected)
        self.assertNotEqual(module.observe(request, self.destination)["result"], expected)

    def test_mutation_guard_rejects_dropped_source_records(self) -> None:
        source = (HERE / "a2a.py").read_text(encoding="utf-8")
        broken = source.replace("result.update(records=records[offset:end],", "result.update(records=[],")
        self.assertNotEqual(source, broken)
        module = importlib.util.module_from_spec(importlib.util.spec_from_loader("a2a_collection_mutant", loader=None))
        exec(compile(broken, "<a2a-collection-mutant>", "exec"), module.__dict__)
        self.assertEqual([value["id"] for value in a2a.read_source(source_request(), self.journal)["records"]], ["s1"])
        with self.assertRaises(AssertionError):
            self.assertEqual([value["id"] for value in module.read_source(source_request(), self.journal)["records"]], ["s1"])


if __name__ == "__main__":
    unittest.main()
