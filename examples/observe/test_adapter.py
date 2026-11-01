import copy
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from typing import Any


ADAPTER = Path(__file__).with_name("adapter.py")
AT = "2026-01-01T00:00:00Z"


def source_request() -> dict[str, Any]:
    return {"protocol": "rindcut.source/v1", "operation": "read", "source_id": "acme", "source_revision": 1,
            "source_instance": "acme", "stream": "actions", "generation": None, "cursor": None, "limit": 100}


def action(native_id: str = "e1") -> dict[str, Any]:
    return {"id": native_id, "type": "acme.unregistered.terminal", "action_id": "A1", "claim": "success",
            "authority": "person", "verified": True, "occurred_at": AT}


def observer_request() -> dict[str, Any]:
    return {"protocol": "rindcut.observer/v1", "source_id": "acme", "source_revision": 1,
            "observer_id": "effect", "observer_revision": 1, "criterion_id": "actual_state",
            "criterion_revision": "C1", "series_id": "S1", "target_ref": {"store_id": "store", "capture_id": "C1"},
            "event": {"action_id": "A1", "attributes": {"declared_claim": "success", "declared_authority": "person",
                                                               "declared_verified": True}},
            "parameters": {"expected_state": "ready"}, "now": AT}


class AdapterTest(unittest.TestCase):
    def invoke(self, request: Any, contents: dict[str, Any], source_format: str = "acme",
               mode: str = "reader", raw: bool = False) -> subprocess.CompletedProcess[str]:
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "input.json"
            path.write_text(json.dumps(contents), encoding="utf-8")
            encoded = request if raw else json.dumps(request)
            command = [sys.executable, str(ADAPTER), mode, "--format", source_format, "--input", str(path)]
            result = subprocess.run(command,
                                    input=encoded, text=True, capture_output=True, timeout=5, check=False)
        return result

    def success(self, request: dict[str, Any], contents: dict[str, Any], source_format: str = "acme",
                mode: str = "reader") -> dict[str, Any]:
        result = self.invoke(request, contents, source_format, mode)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stderr, "")
        return json.loads(result.stdout)

    def test_metadata_projection(self) -> None:
        cases = [
            ("acme", [action()], "acme.unregistered.terminal", "event_stream"),
            ("nodloop", [{"record_id": "n1", "record_kind": "verdict", "schema_version": 1, "time": AT,
                          "verdict": "approve", "authority": "person", "proof": "none", "trace_id": "A1",
                          "reviewer": "ann", "output": "private text", "dir": "/private/path"}],
             "nodloop.verdict", "event_stream"),
            ("meetproxy", [{"sample_id": "s1", "sampled_at": AT, "posts": [
                {"request": "A1", "reply": "https://example.com/reply", "body": "private text", "at": AT}]}],
             "meetproxy.post.snapshot", "snapshot"),
        ]
        for source_format, records, event_type, capability in cases:
            with self.subTest(source_format=source_format):
                result = self.success(source_request(), {"generation": "g1", "records": records}, source_format)
                self.assertEqual(result["records"][0]["type"], event_type)
                self.assertEqual(result["capability"]["kind"], capability)
                public = json.dumps(result)
                for excluded in ("private text", "/private/path", "https://example.com/reply", "reviewer", "landed"):
                    self.assertNotIn(excluded, public)

    def test_pagination_late_append_and_invalid_cursor(self) -> None:
        request = source_request()
        request["limit"] = 1
        journal = {"generation": "g1", "records": [action()]}
        first = self.success(request, journal)
        request["cursor"], request["generation"] = first["next_cursor"], "g1"
        empty = self.success(request, journal)
        self.assertEqual(empty["records"], [])
        self.assertEqual(empty["next_cursor"], request["cursor"])
        late = action("e2")
        late["occurred_at"] = "2025-01-01T00:00:00Z"
        journal["records"].append(late)
        second = self.success(request, journal)
        self.assertEqual([record["id"] for record in second["records"]], ["e2"])
        cases = [("prefix changed", {"generation": "g1", "records": [action("changed")]}),
                 ("generation changed", {"generation": "g2", "records": [action()]})]
        for name, changed in cases:
            with self.subTest(name=name):
                result = self.success(request, changed)
                self.assertEqual(result["status"], "cursor_invalid")
                self.assertEqual(result["records"], [])
                self.assertIsNone(result["next_cursor"])

    def test_snapshot_replay_and_new_sample(self) -> None:
        sample = {"sample_id": "s1", "sampled_at": AT, "posts": [{"request": "A1", "reply": "r1"}]}
        journal = {"generation": "g1", "records": [sample]}
        request = source_request()
        first = self.success(request, journal, "meetproxy")
        replay = self.success(request, journal, "meetproxy")
        self.assertEqual(first, replay)
        request["cursor"], request["generation"] = first["next_cursor"], "g1"
        self.assertEqual(self.success(request, journal, "meetproxy")["records"], [])
        next_sample = copy.deepcopy(sample)
        next_sample["sample_id"] = "s2"
        journal["records"].append(next_sample)
        second = self.success(request, journal, "meetproxy")
        self.assertEqual(len(second["records"]), 1)
        self.assertNotEqual(first["records"][0]["id"], second["records"][0]["id"])
        self.assertIn("no_intermediate_attempts", second["coverage"]["limitations"])

    def test_nodloop_identity_collision_preserved(self) -> None:
        records = [{"record_id": "n1", "record_kind": "verdict", "schema_version": 1, "verdict": value}
                   for value in ("approve", "reject")]
        result = self.success(source_request(), {"generation": "g1", "records": records}, "nodloop")
        self.assertEqual([record["id"] for record in result["records"]], ["n1", "n1"])
        self.assertEqual([record["attributes"]["declared_verdict"] for record in result["records"]],
                         ["approve", "reject"])

    def test_actual_destination_and_explicit_correction(self) -> None:
        request = observer_request()
        cases = [({"effects": {}}, "pending", False),
                 ({"effects": {"A1": {"state": "ready"}}}, "confirmed", False),
                 ({"effects": {"A1": {"state": "wrong"}}}, "refuted", False),
                 ({"effects": {"A1": {"state": "wrong"}}, "correct_previous": True}, "refuted", True)]
        request["previous_observation_id"] = "O1"
        for destination, expected, corrected in cases:
            with self.subTest(expected=expected, corrected=corrected):
                result = self.success(request, destination, mode="observer")
                self.assertEqual(result["result"], expected)
                self.assertEqual("supersedes" in result, corrected)
                self.assertNotIn("authority", result)
                self.assertNotIn("verified", result)
                if corrected:
                    self.assertEqual(result["supersedes"], {"store_id": "store", "observation_id": "O1"})

    def test_invalid_inputs_do_not_leak(self) -> None:
        request = source_request()
        cases = [
            (json.dumps(request)[:-1] + ',"source_id":"private token"}', {"generation": "g1", "records": []}, "acme"),
            (json.dumps(request), {"generation": "g1", "records": [{"record_id": "n1", "record_kind": "run",
                                                         "schema_version": 2, "output": "private token"}]}, "nodloop"),
            (json.dumps(request), {"generation": "g1", "records": [{"record_id": "n1", "record_kind": "guard",
                                                                     "schema_version": 1}]}, "nodloop"),
        ]
        for wire, journal, source_format in cases:
            with self.subTest(source_format=source_format, wire=wire):
                result = self.invoke(wire, journal, source_format, raw=True)
                self.assertEqual(result.returncode, 1)
                self.assertEqual(result.stdout, "")
                self.assertEqual(result.stderr, "example_adapter_failed\n")

    def test_invalid_observer_shapes_do_not_leak(self) -> None:
        cases = [({}, {"effects": {}}),
                 (observer_request(), {"effects": []}),
                 (observer_request(), {"effects": {"A1": None}, "correct_previous": "private token"})]
        for request, destination in cases:
            with self.subTest(request=request, destination=destination):
                result = self.invoke(request, destination, mode="observer")
                self.assertEqual(result.returncode, 1)
                self.assertEqual(result.stdout, "")
                self.assertEqual(result.stderr, "example_adapter_failed\n")


if __name__ == "__main__":
    unittest.main()
