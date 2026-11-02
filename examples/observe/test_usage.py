import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from typing import Any
from unittest import mock

import usage


HERE = Path(__file__).resolve().parent
FIXTURES = json.loads((HERE / "usage_fixtures.json").read_text(encoding="utf-8"))
EXPECTED = json.loads((HERE / "usage_expected_manifest.json").read_text(encoding="utf-8"))
AT = "2026-01-01T00:00:00Z"


def digest(value: Any) -> str:
    data = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False).encode()
    return "sha256:" + hashlib.sha256(data).hexdigest()


def example() -> tuple[dict[str, Any], dict[str, Any], dict[str, Any]]:
    scope = {"provider": "openai", "api_version": "chat.completions.v1", "model": "acme-model",
             "meter_profile": "openai.chat.provider.v1"}
    budget = dict(scope, id="B1", revision="1", comparison="inclusive", missing="unknown",
                  units={"input_tokens": "tokens", "output_tokens": "tokens", "tool_calls": "calls",
                         "elapsed_ms": "milliseconds"},
                  limits={"input_tokens": 200, "output_tokens": 40, "tool_calls": 2, "elapsed_ms": 500})
    native = copy.deepcopy(FIXTURES["vectors"][0]["native"])
    first = dict(scope, id="R1", version="1", run_ref="run-1", attempt_id="A1", request_ref="Q1",
                 accounting_plane="provider", usage_scope="inclusive", native=native)
    second = dict(scope, id="R2", version="1", run_ref="run-1", attempt_id="A2", request_ref="Q2",
                  accounting_plane="provider", usage_scope="exclusive", native=copy.deepcopy(native))
    proxy = dict(first, id="R-proxy", accounting_plane="proxy", duplicate_of="R1")
    child = dict(scope, id="R-child", version="1", run_ref="run-1", attempt_id="A-child", request_ref="Q-child",
                 accounting_plane="provider", usage_scope="exclusive", included_in="R1",
                 native={"kind": "openai.chat.completion", "usage": {
                     "prompt_tokens": 30, "completion_tokens": 5, "prompt_tokens_details": {"cached_tokens": 10},
                     "completion_tokens_details": {"reasoning_tokens": 2}}})
    receipts = [first, second, proxy, child, copy.deepcopy(first)]
    refs = [{"id": item["id"], "version": item["version"], "digest": digest(item)} for item in receipts]
    manifest = dict(scope, id="M-final", snapshot_id="S-final", run_ref="run-1", budget_id="B1",
                    budget_revision="1", complete=True, primary_accounting_plane="provider",
                    otel_revision=FIXTURES["otel_revision"], expected_attempts=["A1", "A2", "A-child"],
                    receipts=refs, clock={"start": {"clock": "C1", "ms": 100},
                                          "end": {"clock": "C1", "ms": 600}}, tool_calls=[
                        {"id": "T1", "run_ref": "run-1", "scope": "direct_client", "status": "success"},
                        {"id": "T2", "run_ref": "run-1", "scope": "direct_client", "status": "failed"},
                        {"id": "T-child", "run_ref": "run-1", "scope": "subagent", "status": "success"},
                        {"id": "T-internal", "run_ref": "run-1", "scope": "provider_internal", "status": "success"},
                        {"id": "T1", "run_ref": "run-1", "scope": "direct_client", "status": "success"}])
    final = dict(scope, snapshot_id="S-final", run_ref="run-1", budget_id="B1", budget_revision="1",
                 budget_digest=digest(budget), manifest_id="M-final", manifest_digest=digest(manifest),
                 usage_complete=True, usage={"input_tokens": 200, "output_tokens": 40,
                                            "cached_input_tokens": 120, "reasoning_output_tokens": 16,
                                            "tool_calls": 2, "elapsed_ms": 500},
                 replaces={"snapshot_id": "S-partial", "type": "acme.run_usage.partial", "generation": "g1"})
    partial = dict(scope, snapshot_id="S-partial", run_ref="run-1", budget_id="B1", budget_revision="1",
                   budget_digest=digest(budget), manifest_id="M-partial", manifest_digest=digest({"id": "M-partial"}),
                   usage_complete=False, usage={"input_tokens": 100, "output_tokens": 10, "tool_calls": 1,
                                                "elapsed_ms": 100, "cached_input_tokens": 60,
                                                "reasoning_output_tokens": 4})
    parameters = {"expected_" + key: value for key, value in scope.items()}
    parameters.update(expected_budget_id="B1", expected_budget_revision="1", expected_budget_digest=digest(budget))
    return {"generation": "g1", "snapshots": [partial, final]}, {
        "budgets": [budget], "manifests": [manifest], "receipts": receipts, "preparing": []}, parameters


def source_request() -> dict[str, Any]:
    return {"protocol": "rindcut.source/v1", "operation": "read", "source_id": "usage", "source_revision": 1,
            "source_instance": "acme", "stream": "usage", "generation": None, "cursor": None, "limit": 100}


def observer_request(event: dict[str, Any], parameters: dict[str, Any]) -> dict[str, Any]:
    return {"protocol": "rindcut.observer/v1", "source_id": "usage", "source_revision": 1,
            "observer_id": "budget", "observer_revision": 1, "criterion_id": "usage_within_budget",
            "criterion_revision": "C1", "series_id": "series-1", "target_ref": {"store_id": "store", "capture_id": "capture"},
            "event": event, "parameters": parameters, "now": AT}


def inclusion_case(name: str) -> tuple[dict[str, Any], dict[str, Any], dict[str, Any]]:
    journal, resolver, parameters = example()
    vector = next(value for value in FIXTURES["inclusion_vectors"] if value["id"] == name)
    template = next(receipt for receipt in resolver["receipts"] if receipt["id"] == "R-child")
    resolver["receipts"] = [receipt for receipt in resolver["receipts"] if receipt["id"] != "R-child"]
    for index, values in enumerate(vector["children"]):
        child = dict(copy.deepcopy(template), id=f"R-child-{index}", attempt_id=f"A-child-{index}",
                     request_ref=values["request_ref"], usage_scope=values.get("usage_scope", "exclusive"))
        child["native"]["usage"] = {
            "prompt_tokens": values["input_tokens"], "completion_tokens": values["output_tokens"],
            "prompt_tokens_details": {"cached_tokens": values["cached_input_tokens"]},
            "completion_tokens_details": {"reasoning_tokens": values["reasoning_output_tokens"]}}
        resolver["receipts"].append(child)
    manifest = resolver["manifests"][0]
    manifest["expected_attempts"] = ["A1", "A2"] + [f"A-child-{index}" for index in range(len(vector["children"]))]
    manifest["receipts"] = [{"id": receipt["id"], "version": receipt["version"], "digest": digest(receipt)}
                            for receipt in resolver["receipts"]]
    journal["snapshots"][-1]["manifest_digest"] = digest(manifest)
    return journal, resolver, parameters


class UsageTest(unittest.TestCase):
    def setUp(self) -> None:
        self.journal, self.resolver, self.parameters = example()
        self.page = usage.read_source(source_request(), self.journal)
        self.event = self.page["records"][-1]
        self.request = observer_request(self.event, self.parameters)

    def result(self) -> dict[str, Any]:
        return usage.observe(self.request, self.resolver)

    def rebind(self) -> None:
        manifest = self.resolver["manifests"][0]
        receipts = {(item["id"], item["version"]): item for item in self.resolver["receipts"]}
        for ref in manifest["receipts"]:
            if (ref["id"], ref["version"]) in receipts:
                ref["digest"] = digest(receipts[(ref["id"], ref["version"])])
        self.event["evidence"][0]["digest"] = digest(manifest)
        self.event["evidence"][1]["digest"] = digest(self.resolver["budgets"][0])
        self.parameters["expected_budget_digest"] = self.event["evidence"][1]["digest"]

    def test_literal_native_vectors(self) -> None:
        for vector in FIXTURES["vectors"]:
            with self.subTest(vector=vector["id"]):
                actual, complete = usage.normalize(vector["native"], vector["provider"])
                self.assertEqual(actual, vector["expected"])
                self.assertEqual(complete, vector["complete"])

    def test_multi_child_inclusion_matches_literal_manifest(self) -> None:
        for vector in FIXTURES["inclusion_vectors"]:
            with self.subTest(vector=vector["id"]):
                journal, resolver, parameters = inclusion_case(vector["id"])
                event = usage.read_source(source_request(), journal)["records"][-1]
                result = usage.observe(observer_request(event, parameters), resolver)
                expected = EXPECTED["inclusion_results"][vector["id"]]
                self.assertEqual(result["result"], expected)
                if expected == "confirmed":
                    self.assertEqual(usage.aggregate(resolver["manifests"][0], resolver["receipts"]),
                                     EXPECTED["normalized_run"])

    def test_exact_inclusive_budget_and_actual_retry(self) -> None:
        self.assertEqual(usage.aggregate(self.resolver["manifests"][0], self.resolver["receipts"]),
                         EXPECTED["normalized_run"])
        result = self.result()
        self.assertEqual(result["result"], EXPECTED["boundary_result"])
        self.assertEqual(result["reason_code"], "within_budget")
        self.assertEqual(result["target_ref"], self.request["target_ref"])
        for forbidden in ("authority", "verified", "input_tokens", "usage", "supersedes"):
            self.assertNotIn(forbidden, result)

    def anthropic_receipts(self) -> None:
        scope = {"provider": "anthropic", "api_version": "messages.2023-06-01", "model": "acme-model",
                 "meter_profile": "anthropic.messages.provider.v1"}
        self.event["attributes"].update(scope)
        self.event["attributes"].pop("reasoning_output_tokens")
        self.parameters.update({"expected_" + name: value for name, value in scope.items()})
        self.resolver["budgets"][0].update(scope)
        self.resolver["manifests"][0].update(scope)
        for receipt in self.resolver["receipts"]:
            receipt.update(scope)
            receipt["native"] = copy.deepcopy(FIXTURES["vectors"][1]["native"])
            if receipt["id"] == "R-child":
                receipt["native"]["usage"].update(cache_creation_input_tokens=10, cache_read_input_tokens=10,
                                                  output_tokens=5)
        self.rebind()

    def test_anthropic_receipts_use_separate_cache_normalization(self) -> None:
        self.anthropic_receipts()
        expected = {key: value for key, value in EXPECTED["normalized_run"].items() if key != "reasoning_output_tokens"}
        self.assertEqual(usage.aggregate(self.resolver["manifests"][0], self.resolver["receipts"]), expected)
        self.assertEqual(self.result()["result"], "confirmed")

    def test_anthropic_incomplete_stream_cannot_confirm_final_projection(self) -> None:
        names = ("anthropic-stop-without-final-usage", "anthropic-stream-error")
        for name in names:
            with self.subTest(vector=name):
                self.setUp()
                self.anthropic_receipts()
                vector = next(value for value in FIXTURES["vectors"] if value["id"] == name)
                self.resolver["receipts"][1]["native"] = copy.deepcopy(vector["native"])
                self.event["attributes"]["output_tokens"] = 21 if name.endswith("final-usage") else 40
                self.rebind()
                self.assertEqual(self.result()["result"], "unknown")
                self.assertEqual(self.result()["reason_code"], "receipt_incomplete")

    def test_openai_stream_error_boundary_matches_literal_manifest(self) -> None:
        for name, expected in EXPECTED["stream_results"].items():
            with self.subTest(vector=name):
                self.setUp()
                vector = next(value for value in FIXTURES["vectors"] if value["id"] == name)
                self.resolver["receipts"][1]["native"] = copy.deepcopy(vector["native"])
                self.rebind()
                result = self.result()
                self.assertEqual(result["result"], expected["result"])
                self.assertEqual(result["reason_code"], expected["reason_code"])

    def test_falsey_stream_error_and_separate_final_receipt_remain_usable(self) -> None:
        vector = next(value for value in FIXTURES["vectors"] if value["id"] == "openai-empty-error")
        for error in (None, False, 0, "", [], {}):
            with self.subTest(error=error):
                native = copy.deepcopy(vector["native"])
                native["chunks"][0]["error"] = error
                self.assertEqual(usage.normalize(native, "openai"), (vector["expected"], True))
        self.resolver["receipts"][1]["native"]["metadata"] = {"application_error": "business effect failed"}
        self.rebind()
        self.assertEqual(self.result()["result"], "confirmed")

    def test_each_exceeded_dimension(self) -> None:
        for name in ("input_tokens", "output_tokens", "tool_calls", "elapsed_ms"):
            with self.subTest(dimension=name):
                self.setUp()
                self.resolver["budgets"][0]["limits"][name] -= 1
                self.rebind()
                self.assertEqual(self.result()["result"], EXPECTED["exceeded_result"])

    def test_final_projection_missing_dimension_and_boolean_are_unknown(self) -> None:
        for value in (None, True, 1.5, -1, 9007199254740992):
            with self.subTest(value=value):
                self.setUp()
                if value is None:
                    del self.event["attributes"]["input_tokens"]
                else:
                    self.event["attributes"]["input_tokens"] = value
                self.assertEqual(self.result()["result"], EXPECTED["missing_dimension_result"])

    def test_fixed_receipt_preparing_then_available(self) -> None:
        manifest = self.resolver["manifests"].pop()
        binding = {"id": "M-final", "digest": digest(manifest), "snapshot_id": "S-final", "run_ref": "run-1",
                   "budget_id": "B1", "budget_revision": "1"}
        binding.update({name: self.event["attributes"][name] for name in usage.SCOPE})
        self.resolver["preparing"] = [binding]
        first = self.result()
        self.resolver["manifests"].append(manifest)
        second = self.result()
        self.assertEqual([first["result"], second["result"]], EXPECTED["preparing_results"])
        self.assertEqual(first["target_ref"], second["target_ref"])
        self.assertNotIn("supersedes", second)
        self.resolver["manifests"] = []
        self.resolver["preparing"][0]["snapshot_id"] = "another-snapshot"
        self.assertEqual(self.result()["result"], "unknown")

    def test_deadline_does_not_refute_missing_evidence(self) -> None:
        self.resolver["manifests"] = []
        self.request["now"] = "2030-01-01T00:00:00Z"
        self.assertEqual(self.result()["result"], "unknown")

    def test_policy_and_evidence_mismatches_unknown(self) -> None:
        changes = [
            lambda: self.event["attributes"].update(budget_revision="2"),
            lambda: self.event["attributes"].update(meter_profile="another-meter"),
            lambda: self.event["attributes"].update(provider="anthropic"),
            lambda: self.event["attributes"].update(api_version="chat.completions.v2"),
            lambda: self.event["attributes"].update(model="another-model"),
            lambda: self.event["evidence"][0].update(digest="sha256:" + "0" * 64),
            lambda: self.event["evidence"][1].update(digest="sha256:" + "0" * 64),
            lambda: self.resolver["receipts"][0]["native"]["usage"].update(prompt_tokens=999),
            lambda: self.resolver["manifests"][0].update(run_ref="another-run"),
            lambda: self.resolver["budgets"][0]["units"].update(elapsed_ms="seconds"),
        ]
        for index, change in enumerate(changes):
            with self.subTest(change=index):
                self.setUp()
                change()
                self.assertEqual(self.result()["result"], "unknown")

    def test_partial_is_not_final_or_selectable(self) -> None:
        self.assertEqual([event["type"] for event in self.page["records"]], EXPECTED["event_types"])
        self.assertEqual([event["id"] for event in self.page["records"]], EXPECTED["event_ids"])
        partial = self.page["records"][0]
        self.assertEqual(usage.observe(observer_request(partial, self.parameters), self.resolver)["result"], "unknown")
        captures = [dict(event, source_id="usage") for event in self.page["records"]]
        self.assertEqual(usage.select_snapshot(captures)["id"], EXPECTED["selected_snapshot"])
        with self.assertRaises(ValueError):
            usage.select_snapshot([captures[0]])
        captures[-1]["links"] = []
        with self.assertRaises(ValueError):
            usage.select_snapshot(captures)

    def test_snapshot_chain_scope_cycles_and_unresolved_links(self) -> None:
        for kind in ("other_source", "other_run", "cycle", "unresolved", "scope", "fork"):
            with self.subTest(kind=kind):
                self.setUp()
                events = [dict(event, source_id="usage") for event in self.page["records"]]
                if kind == "other_source":
                    events[0]["source_instance"] = "another-source"
                elif kind == "other_run":
                    events[0]["action_id"] = "another-run"
                elif kind == "cycle":
                    events[0]["links"] = [{"relation": "replaces_snapshot", "target": dict(
                        self.event["links"][0]["target"], id="S-final", type=usage.FINAL)}]
                elif kind == "unresolved":
                    self.event["links"][0]["target"]["id"] = "absent"
                elif kind == "scope":
                    events[0]["attributes"]["budget_revision"] = "2"
                else:
                    events.append(dict(copy.deepcopy(self.event), id="another-final"))
                with self.assertRaises(ValueError):
                    usage.select_snapshot(events)

    def test_snapshot_selection_requires_capture_source_identity(self) -> None:
        with self.assertRaises(ValueError):
            usage.select_snapshot(self.page["records"])
        captures = [dict(event, source_id="usage") for event in self.page["records"]]
        captures[-1]["links"][0]["target"]["source_id"] = "foreign-source"
        with self.assertRaises(ValueError):
            usage.select_snapshot(captures)

    def test_provider_request_identity_does_not_change_with_attempt(self) -> None:
        self.resolver["receipts"][1]["request_ref"] = "Q1"
        self.rebind()
        self.assertEqual(self.result()["result"], "unknown")

    def test_replay_pagination_append_generation_and_identity_conflict(self) -> None:
        request = source_request()
        request["limit"] = 1
        first = usage.read_source(request, self.journal)
        self.assertEqual(first, usage.read_source(request, self.journal))
        request.update(generation="g1", cursor=first["next_cursor"])
        second = usage.read_source(request, self.journal)
        self.assertEqual(second["records"][0]["id"], "S-final")
        request["cursor"] = second["next_cursor"]
        empty = usage.read_source(request, self.journal)
        self.assertEqual(empty["records"], [])
        self.assertEqual(empty["next_cursor"], request["cursor"])
        late = copy.deepcopy(self.journal["snapshots"][-1])
        late["snapshot_id"] = "S-late"
        late["occurred_at"] = "2025-01-01T00:00:00Z"
        self.journal["snapshots"].append(late)
        self.assertEqual(usage.read_source(request, self.journal)["records"][0]["id"], "S-late")
        self.journal["generation"] = "g2"
        self.assertEqual(usage.read_source(request, self.journal)["status"], "cursor_invalid")
        self.journal["generation"] = "g1"
        self.journal["snapshots"][0]["usage"]["input_tokens"] = 101
        self.assertEqual(usage.read_source(request, self.journal)["status"], "cursor_invalid")
        changed = copy.deepcopy(self.journal["snapshots"][-1])
        changed["usage"]["input_tokens"] = 999
        self.journal["snapshots"].append(changed)
        records = usage.read_source(source_request(), self.journal)["records"]
        self.assertEqual(records[-1]["id"], records[-2]["id"])
        self.assertNotEqual(records[-1]["attributes"], records[-2]["attributes"])

    def test_accounting_plane_inclusion_coverage_and_clock_fail_closed(self) -> None:
        changes = [
            lambda: self.resolver["receipts"][2].pop("duplicate_of"),
            lambda: self.resolver["receipts"][2].update(request_ref="different-request"),
            lambda: self.resolver["receipts"][3].update(included_in="missing-parent"),
            lambda: self.resolver["manifests"][0].update(expected_attempts=["A1"]),
            lambda: self.resolver["manifests"][0].update(otel_revision="newest"),
            lambda: self.resolver["manifests"][0]["clock"]["end"].update(clock="C2"),
            lambda: self.resolver["manifests"][0]["clock"]["end"].update(ms=99),
            lambda: self.resolver["manifests"][0]["tool_calls"][1].update(scope="unknown"),
        ]
        for index, change in enumerate(changes):
            with self.subTest(change=index):
                self.setUp()
                change()
                self.rebind()
                self.assertEqual(self.result()["result"], "unknown")

    def test_receipt_versions_and_redelivery_conflicts_are_unknown(self) -> None:
        for kind in ("missing_version", "conflicting_redelivery", "unbound_duplicate", "child_not_inclusive"):
            with self.subTest(kind=kind):
                self.setUp()
                if kind == "missing_version":
                    self.resolver["manifests"][0]["receipts"][1]["version"] = "2"
                elif kind == "conflicting_redelivery":
                    self.resolver["receipts"][-1]["request_ref"] = "changed-request"
                elif kind == "unbound_duplicate":
                    duplicate = copy.deepcopy(self.resolver["receipts"][1])
                    duplicate["id"] = "R-unbound"
                    self.resolver["receipts"].append(duplicate)
                    self.resolver["manifests"][0]["receipts"].append({"id": duplicate["id"], "version": "1",
                                                                    "digest": digest(duplicate)})
                else:
                    for receipt in self.resolver["receipts"]:
                        if receipt["id"] == "R1":
                            receipt["usage_scope"] = "exclusive"
                self.rebind()
                self.assertEqual(self.result()["result"], "unknown")

    def test_checked_in_examples_match_literal_manifest(self) -> None:
        journal = json.loads((HERE / "usage_journal.json").read_text(encoding="utf-8"))
        resolver = json.loads((HERE / "usage_resolver.json").read_text(encoding="utf-8"))
        parameters = json.loads((HERE / "usage_parameters.json").read_text(encoding="utf-8"))
        definition = json.loads((HERE / "usage_definition.json").read_text(encoding="utf-8"))
        self.assertEqual((journal, resolver, parameters), example())
        self.assertEqual(definition["capability"], usage.CAPABILITY)
        self.assertEqual(definition["observers"][0]["types"], [usage.FINAL])
        self.assertEqual(definition["reader"]["allowed_attributes"], list(usage.ATTRIBUTES))
        events = usage.read_source(source_request(), journal)["records"]
        self.assertEqual(usage.observe(observer_request(events[-1], parameters), resolver)["result"], "confirmed")

    def test_missing_native_usage_is_not_zero(self) -> None:
        self.resolver["receipts"] = [receipt for receipt in self.resolver["receipts"] if receipt["id"] != "R1"]
        self.assertEqual(self.result()["result"], "unknown")
        self.setUp()
        for receipt in self.resolver["receipts"]:
            if receipt["id"] == "R2":
                receipt["native"] = copy.deepcopy(FIXTURES["vectors"][3]["native"])
        self.rebind()
        self.assertEqual(self.result()["result"], "unknown")

    def test_security_projection_and_no_input_selected_io(self) -> None:
        for snapshot in self.journal["snapshots"]:
            snapshot.update(prompt="private prompt", token="private-token", url="https://example.com/private",
                            program="arbitrary-program", authority="person", verified=True)
            snapshot["usage"]["raw"] = {"secret": "private payload"}
        with mock.patch("pathlib.Path.open", side_effect=AssertionError("unexpected IO")), \
             mock.patch("subprocess.run", side_effect=AssertionError("unexpected execution")):
            page = usage.read_source(source_request(), self.journal)
            result = usage.observe(observer_request(page["records"][-1], self.parameters), self.resolver)
        public = json.dumps([page, result])
        for forbidden in ("private prompt", "private-token", "https://", "arbitrary-program", "private payload", "authority", "verified"):
            self.assertNotIn(forbidden, public)
        self.assertEqual(set(page["records"][-1]["attributes"]), set(usage.ATTRIBUTES))
        self.journal["snapshots"][-1]["manifest_id"] = "https://example.com/private"
        with self.assertRaises(ValueError):
            usage.read_source(source_request(), self.journal)

    def test_allowed_attribute_values_reject_urls_and_whitespace(self) -> None:
        cases = (("model", "https://example.com/private"), ("meter_profile", "has space"))
        for name, value in cases:
            with self.subTest(attribute=name), tempfile.TemporaryDirectory() as folder:
                journal = copy.deepcopy(self.journal)
                journal["snapshots"][-1][name] = value
                with self.assertRaisesRegex(usage.EvidenceError, "^invalid_reference$"):
                    usage.read_source(source_request(), journal)
                path = Path(folder) / "input.json"
                path.write_text(json.dumps(journal), encoding="utf-8")
                command = [sys.executable, "-B", str(HERE / "usage.py"), "reader", "--input", str(path)]
                result = subprocess.run(command, input=json.dumps(source_request()), text=True,
                                        capture_output=True, check=False, timeout=5)
                self.assertEqual(result.returncode, 1)
                self.assertEqual(result.stdout, "")
                self.assertEqual(result.stderr, "usage_adapter_failed\n")

    def test_stdio_fixed_errors_no_payload_echo(self) -> None:
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "input.json"
            path.write_text(json.dumps(self.journal), encoding="utf-8")
            command = [sys.executable, str(HERE / "usage.py"), "reader", "--input", str(path)]
            good = subprocess.run(command, input=json.dumps(source_request()), text=True, capture_output=True, check=False, timeout=5)
            self.assertEqual(good.returncode, 0, good.stderr)
            self.assertEqual(json.loads(good.stdout), self.page)
            bad = subprocess.run(command, input='{"protocol":"private-payload","protocol":"duplicate"}',
                                 text=True, capture_output=True, check=False, timeout=5)
            self.assertEqual(bad.returncode, 1)
            self.assertEqual(bad.stdout, "")
            self.assertEqual(bad.stderr, "usage_adapter_failed\n")

    def test_native_counter_and_subset_validation(self) -> None:
        for value in (-1, True, 1.5, 9007199254740992):
            with self.subTest(value=value):
                native = copy.deepcopy(FIXTURES["vectors"][0]["native"])
                native["usage"]["prompt_tokens"] = value
                with self.assertRaises(ValueError):
                    usage.normalize(native, "openai")
        native = copy.deepcopy(FIXTURES["vectors"][0]["native"])
        native["usage"]["prompt_tokens_details"]["cached_tokens"] = 101
        with self.assertRaises(ValueError):
            usage.normalize(native, "openai")
        native = copy.deepcopy(FIXTURES["vectors"][4]["native"])
        native["events"][2]["usage"]["output_tokens"] = 1
        with self.assertRaises(ValueError):
            usage.normalize(native, "anthropic")

    def test_mutations_are_rejected_by_literal_expectations(self) -> None:
        original = (HERE / "usage.py").read_text(encoding="utf-8")
        mutations = {
            "cached_double_count": ("result[normalized] = integer(usage[details][native])",
                                    "result[normalized] = integer(usage[details][native]); result[total] += result[normalized]",
                                    "test_checked_in_examples_match_literal_manifest"),
            "anthropic_stream_sum": ("usage[name] = latest", "usage[name] = integer(usage.get(name, 0)) + latest",
                                     "test_literal_native_vectors"),
            "boundary_exclusive": ("integer(metrics[name]) > maximum", "integer(metrics[name]) >= maximum",
                                   "test_exact_inclusive_budget_and_actual_retry"),
            "skip_real_retry": ("roots = accounting_roots(selected)", "roots = accounting_roots(selected)[:1]",
                                "test_exact_inclusive_budget_and_actual_retry"),
            "skip_child_sum": ("if known > maximum:", "if False:",
                               "test_multi_child_inclusion_matches_literal_manifest"),
            "ignore_request_identity": ("if request in requests:", "if False:",
                                        "test_provider_request_identity_does_not_change_with_attempt"),
            "missing_final_usage": ("final_usage = bool(deltas) and", "final_usage = not deltas or",
                                    "test_literal_native_vectors"),
            "ignore_stream_error": ('failed = any(event["type"] == "error" for event in events)',
                                    "failed = False", "test_literal_native_vectors"),
            "ignore_openai_stream_error": ('if any(chunk.get("error") for chunk in native["chunks"]):',
                                           'if False:', "test_openai_stream_error_boundary_matches_literal_manifest"),
            "reject_nested_openai_error": ('chunk.get("error") for chunk in native["chunks"]',
                                           '"error" in str(chunk) for chunk in native["chunks"]',
                                           "test_openai_stream_error_boundary_matches_literal_manifest"),
            "reject_falsey_openai_error": ('chunk.get("error") for chunk in native["chunks"]',
                                           '"error" in chunk for chunk in native["chunks"]',
                                           "test_falsey_stream_error_and_separate_final_receipt_remain_usable"),
            "source_identity_fallback": ('opaque(value.get("source_id"))',
                                         'value.setdefault("source_id", "usage")',
                                         "test_snapshot_selection_requires_capture_source_identity"),
            "foreign_source_link": ('("source_id", "source_instance", "stream", "projection_profile"))',
                                    '("source_instance", "stream", "projection_profile"))',
                                    "test_snapshot_selection_requires_capture_source_identity"),
        }
        for name, (before, after, target) in mutations.items():
            with self.subTest(mutation=name), tempfile.TemporaryDirectory() as folder:
                self.assertIn(before, original)
                path = Path(folder) / "mutant.py"
                path.write_text(original.replace(before, after, 1), encoding="utf-8")
                spec = importlib.util.spec_from_file_location("usage_mutant", path)
                module = importlib.util.module_from_spec(spec)
                spec.loader.exec_module(module)
                result = unittest.TestResult()
                with mock.patch(__name__ + ".usage", module):
                    UsageTest(target).run(result)
                self.assertFalse(result.wasSuccessful(), "mutation escaped the acceptance test: " + name)
                self.assertFalse(result.errors, "mutation must fail an assertion rather than crash: " + name)


if __name__ == "__main__":
    unittest.main()
