import copy
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
from pathlib import Path
import sys
import tempfile
import time
from typing import Any

from scenario import scheduler_calls, tree_hash, write


HERE = Path(__file__).resolve().parent
DIMENSIONS = {"input": "input_tokens", "output": "output_tokens", "tool-calls": "tool_calls", "elapsed": "elapsed_ms"}
SCOPE = ("provider", "api_version", "model", "meter_profile")
PRIVATE = ("private text", "private prompt", "private token", "matrix-private-token", "https://example.com/private",
           "matrix-arbitrary-program", "matrix-private-payload", '"authority"', '"verified"')
TIMEOUT = '''import json
from pathlib import Path
import sys
import time
request = json.load(sys.stdin)
with Path(sys.argv[1]).open("a", encoding="utf-8") as log:
    log.write(json.dumps({"mode": "observer", "source_id": request["source_id"], "event_id": request["event"]["id"]}) + "\\n")
time.sleep(5)
'''


def fixture(name: str) -> dict[str, Any]:
    return json.loads((HERE / name).read_text(encoding="utf-8"))


def digest(value: Any) -> str:
    encoded = json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    return "sha256:" + hashlib.sha256(encoded).hexdigest()


def bind_usage(journal: dict[str, Any], resolver: dict[str, Any], parameters: dict[str, Any]) -> None:
    manifest, budget = resolver["manifests"][0], resolver["budgets"][0]
    available = {(item["id"], item["version"]): item for item in resolver["receipts"]}
    for ref in manifest["receipts"]:
        if (ref["id"], ref["version"]) in available:
            ref["digest"] = digest(available[(ref["id"], ref["version"])])
    journal["snapshots"][-1].update(manifest_digest=digest(manifest), budget_digest=digest(budget))
    parameters["expected_budget_digest"] = digest(budget)


def prepare_receipt(journal: dict[str, Any], resolver: dict[str, Any]) -> None:
    snapshot = journal["snapshots"][-1]
    binding = {key: snapshot[key] for key in SCOPE + ("snapshot_id", "run_ref", "budget_id", "budget_revision")}
    binding.update(id=snapshot["manifest_id"], digest=snapshot["manifest_digest"])
    resolver.update(manifests=[], preparing=[binding])


def anthropic_inputs(journal: dict[str, Any], resolver: dict[str, Any], parameters: dict[str, Any],
                     vectors: list[dict[str, Any]], stream: bool = False) -> None:
    scope = {"provider": "anthropic", "api_version": "messages.2023-06-01", "model": "acme-model",
             "meter_profile": "anthropic.messages.provider.v1"}
    snapshot = journal["snapshots"][-1]
    snapshot.update(scope)
    snapshot["usage"].pop("reasoning_output_tokens")
    parameters.update({"expected_" + key: value for key, value in scope.items()})
    resolver["budgets"][0].update(scope)
    resolver["manifests"][0].update(scope)
    for receipt in resolver["receipts"]:
        receipt.update(scope)
        receipt["native"] = copy.deepcopy(vectors[4 if stream else 1]["native"])
        if receipt["id"] == "R-child":
            receipt["native"] = copy.deepcopy(vectors[1]["native"])
            receipt["native"]["usage"].update(cache_creation_input_tokens=10, cache_read_input_tokens=10, output_tokens=5)


def child_inputs(case_id: str, resolver: dict[str, Any], vectors: list[dict[str, Any]]) -> None:
    names = {"normal-disjoint-children": "two-disjoint-children", "child-input-contradiction": "child-input-sum-contradiction",
             "child-output-contradiction": "child-output-sum-contradiction", "child-cache-contradiction": "child-cache-sum-contradiction",
             "child-reasoning-contradiction": "child-reasoning-sum-contradiction", "child-overlap-unknown": "child-overlap-unknown",
             "child-inclusive-scope-unknown": "child-inclusive-scope-unknown"}
    vector = next(item for item in vectors if item["id"] == names[case_id.removeprefix("usage-")])
    template = next(item for item in resolver["receipts"] if item["id"] == "R-child")
    resolver["receipts"] = [item for item in resolver["receipts"] if item["id"] != "R-child"]
    for index, values in enumerate(vector["children"]):
        receipt = dict(copy.deepcopy(template), id=f"R-child-{index}", attempt_id=f"A-child-{index}",
                       request_ref=values["request_ref"], usage_scope=values.get("usage_scope", "exclusive"))
        receipt["native"]["usage"] = {"prompt_tokens": values["input_tokens"], "completion_tokens": values["output_tokens"],
                                      "prompt_tokens_details": {"cached_tokens": values["cached_input_tokens"]},
                                      "completion_tokens_details": {"reasoning_tokens": values["reasoning_output_tokens"]}}
        resolver["receipts"].append(receipt)
    manifest = resolver["manifests"][0]
    manifest["expected_attempts"] = ["A1", "A2"] + [f"A-child-{index}" for index in range(len(vector["children"]))]
    manifest["receipts"] = [{"id": item["id"], "version": item["version"], "digest": digest(item)} for item in resolver["receipts"]]


def a2a_inputs(case_id: str, definition: dict[str, Any]) -> tuple[dict[str, Any], dict[str, Any]]:
    journal, destination = fixture("a2a_snapshots.json"), fixture("a2a_destination.json")
    sample = journal["samples"][0]
    task = next(iter(destination["tasks"].values()))
    parameters = definition["observers"][0]["parameters"]
    if case_id in ("a2a-delayed-pending", "a2a-delayed-then-ready"):
        task.update(complete=False, effects=[])
    elif case_id == "a2a-open-with-effect":
        task["complete"] = False
    elif case_id == "a2a-future-window-not-closed":
        task.update(window_start="2099-01-01T00:00:00Z", window_end="2099-01-01T00:10:00Z")
        task["effects"][0]["occurred_at"] = "2099-01-01T00:01:00Z"
        parameters.update(expected_window_start=task["window_start"], expected_window_end=task["window_end"])
    elif case_id == "a2a-closed-missing-effect":
        task["effects"] = []
    elif case_id == "a2a-closed-missing-artifact":
        task["artifact_state"] = "absent"
    elif case_id == "a2a-artifact-presence-unknown":
        task["artifact_state"] = "unknown"
    elif case_id == "a2a-chunk-gap":
        sample["chunks_complete"] = False
    elif case_id == "a2a-unreadable-destination":
        destination["readable"] = False
    elif case_id == "a2a-fixed-version-unavailable":
        destination["artifacts"] = {}
    elif case_id == "a2a-artifact-digest-mismatch":
        next(iter(destination["artifacts"].values()))["content_base64"] = "d3JvbmcK"
    elif case_id.startswith("a2a-cancel-"):
        sample["task"]["status"]["state"] = "TASK_STATE_CANCELED"
        parameters["cancel_cutoff"] = "2026-01-01T00:01:00Z"
        occurred = {"before-cutoff": "2026-01-01T00:00:30Z", "at-cutoff": "2026-01-01T00:01:00Z",
                    "after-cutoff": "2026-01-01T00:01:30Z"}
        if case_id.endswith("order-unknown"):
            task["effects"][0].pop("occurred_at")
        else:
            task["effects"][0]["occurred_at"] = occurred[case_id.removeprefix("a2a-cancel-")]
    elif case_id in ("a2a-duplicate-known-scope", "a2a-duplicate-scope-unknown", "a2a-no-idempotency-contract"):
        task["effects"].append(dict(task["effects"][0], id="effect-2"))
        if case_id != "a2a-no-idempotency-contract":
            parameters["idempotency_scope"] = "order-1" if case_id.endswith("known-scope") else "order-2"
    elif case_id == "a2a-destination-scope-mismatch":
        destination["scope"]["caller"] = "another-caller"
    elif case_id == "a2a-coverage-window-mismatch":
        task["window_end"] = "2026-01-01T00:09:00Z"
    elif case_id == "a2a-effect-mismatch":
        task["effects"][0]["state"] = "wrong"
    elif case_id != "a2a-normal":
        raise AssertionError("unconfigured A2A case: " + case_id)
    return journal, destination


def usage_inputs(case_id: str, definition: dict[str, Any]) -> tuple[dict[str, Any], dict[str, Any]]:
    journal, resolver = fixture("usage_journal.json"), fixture("usage_resolver.json")
    journal["snapshots"] = [journal["snapshots"][0 if case_id == "usage-partial-record-only" else -1]]
    snapshot = journal["snapshots"][0]
    snapshot.pop("replaces", None)
    snapshot.update(prompt="private prompt", token="matrix-private-token", url="https://example.com/private",
                    program="matrix-arbitrary-program", authority="person", verified=True)
    snapshot["usage"]["raw"] = {"secret": "matrix-private-payload"}
    parameters = definition["observers"][0]["parameters"]
    budget, manifest = resolver["budgets"][0], resolver["manifests"][0]
    vectors = fixture("usage_fixtures.json")
    rebind = False
    if case_id == "usage-below-inclusive-limits":
        budget["limits"] = {key: value + 1 for key, value in budget["limits"].items()}
        rebind = True
    elif case_id.startswith("usage-exceed-"):
        budget["limits"][DIMENSIONS[case_id.removeprefix("usage-exceed-")]] -= 1
        rebind = True
    elif case_id.startswith("usage-missing-"):
        snapshot["usage"].pop(DIMENSIONS[case_id.removeprefix("usage-missing-")])
    elif case_id in ("usage-fixed-receipt-preparing", "usage-preparing-then-ready", "usage-preparing-binding-mismatch",
                     "usage-deadline-pending-overdue"):
        prepare_receipt(journal, resolver)
        if case_id == "usage-preparing-binding-mismatch":
            resolver["preparing"][0]["snapshot_id"] = "another-snapshot"
    elif case_id in ("usage-manifest-missing", "usage-deadline-unknown-overdue"):
        resolver["manifests"] = []
    elif case_id == "usage-receipt-missing":
        resolver["receipts"] = [item for item in resolver["receipts"] if item["id"] != "R2"]
    elif case_id == "usage-receipt-digest-mismatch":
        resolver["receipts"][1]["native"]["usage"]["prompt_tokens"] = 999
    elif case_id in ("usage-manifest-digest-mismatch", "usage-budget-digest-mismatch"):
        snapshot["manifest_digest" if case_id.startswith("usage-manifest") else "budget_digest"] = "sha256:" + "0" * 64
    elif case_id in ("usage-budget-revision-mismatch", "usage-meter-mismatch", "usage-provider-mismatch",
                     "usage-api-mismatch", "usage-model-mismatch"):
        key, value = {"usage-budget-revision-mismatch": ("budget_revision", "2"),
                      "usage-meter-mismatch": ("meter_profile", "another-meter"),
                      "usage-provider-mismatch": ("provider", "anthropic"), "usage-api-mismatch": ("api_version", "chat.completions.v2"),
                      "usage-model-mismatch": ("model", "another-model")}[case_id]
        parameters["expected_" + key] = value
    elif case_id in ("usage-native-receipt-scope-mismatch", "usage-duplicate-provider-request", "usage-invalid-cache-subset"):
        receipt = resolver["receipts"][1]
        if case_id == "usage-native-receipt-scope-mismatch":
            receipt["model"] = "another-model"
        elif case_id == "usage-duplicate-provider-request":
            receipt["request_ref"] = "Q1"
        else:
            receipt["native"]["usage"]["prompt_tokens_details"]["cached_tokens"] = 101
        rebind = True
    elif case_id == "usage-attempt-coverage-mismatch":
        manifest["expected_attempts"].remove("A2")
        rebind = True
    elif case_id == "usage-normal-disjoint-children" or case_id.startswith("usage-child-"):
        child_inputs(case_id, resolver, vectors["inclusion_vectors"])
        rebind = True
    elif case_id.startswith("usage-openai-"):
        vector = next((item for item in vectors["vectors"] if item["id"] == case_id.removeprefix("usage-")), None)
        assert vector is not None, "unconfigured OpenAI case: " + case_id
        resolver["receipts"][1]["native"] = copy.deepcopy(vector["native"])
        rebind = True
    elif case_id.startswith("usage-anthropic-") or case_id == "usage-native-counter-reset":
        anthropic_inputs(journal, resolver, parameters, vectors["vectors"], case_id == "usage-anthropic-cumulative-stream")
        if case_id in ("usage-anthropic-final-usage-missing", "usage-anthropic-stream-error", "usage-native-counter-reset"):
            vector = 7 if case_id.endswith("usage-missing") else 8 if case_id.endswith("stream-error") else 4
            native = copy.deepcopy(vectors["vectors"][vector]["native"])
            if case_id == "usage-native-counter-reset":
                native["events"][2]["usage"]["output_tokens"] = 1
            if case_id == "usage-anthropic-final-usage-missing":
                snapshot["usage"]["output_tokens"] = 21
            resolver["receipts"][1]["native"] = native
        elif case_id not in ("usage-anthropic-cache-components", "usage-anthropic-cumulative-stream"):
            raise AssertionError("unconfigured Anthropic case: " + case_id)
        rebind = True
    elif case_id == "usage-clock-mismatch":
        manifest["clock"]["end"]["clock"] = "C2"
        rebind = True
    elif case_id not in ("usage-exact-inclusive-limits", "usage-partial-record-only", "usage-command-timeout"):
        raise AssertionError("unconfigured usage case: " + case_id)
    if rebind:
        bind_usage(journal, resolver, parameters)
    return journal, resolver


def assert_records(harness: Any, source: str, expected: dict[str, Any]) -> None:
    events = [row["data"] for row in harness.records(source, "event")]
    observations = [row["data"] for row in harness.records(source, "observation")]
    store_id = harness.client.call("observe_status", {})["store_id"]
    assert len(events) == expected["event_count"], "capture count differs"
    assert len(observations) == expected["observation_count"], (
        f"observation count differs: {len(observations)} != {expected['observation_count']}")
    assert len({item["series_id"] for item in observations}) == expected["series_count"], "series count differs"
    assert len({item["capture_id"] for item in events}) == len(events), "duplicate capture identity"
    for event in events:
        for key in ("projection_profile", "source_instance", "generation", "stream"):
            assert event[key] == expected[key], "event identity differs: " + key
        assert event["type"] == expected["event_type"]
        assert event["source_id"] == source and event["source_revision"] == expected["source_revision"]
        if "usage_complete" in expected:
            assert event["attributes"]["usage_complete"] == expected["usage_complete"]
        for key, value in expected.get("metrics", {}).items():
            assert event["attributes"].get(key) == value, "normalized metric differs: " + key
        assert not set(expected.get("absent_attributes", [])).intersection(event["attributes"])
    for observation in observations:
        assert observation["target_ref"] == {"store_id": store_id, "capture_id": events[0]["capture_id"]}
        for key in ("source_revision", "capture_source_revision", "observer_revision", "criterion_revision", "actor_kind"):
            assert observation[key] == expected[key], "observation identity differs: " + key
        assert observation["source_id"] == source
        assert not {"authority", "verified", "supersedes"}.intersection(observation)
    if observations:
        assert observations[-1]["result"] == expected["result"], "result differs"
        assert observations[-1]["reason_code"] == expected["reason_code"], "reason differs"
    calls = [call for call in harness.calls_so_far() if call["mode"] == "observer"]
    assert len(calls) == expected["observer_calls"], "observer call count differs"
    assert all(call["event_id"] == events[0]["id"] for call in calls)
    assert all(call.get("event_type") != "acme.run_usage.partial" for call in calls)
    public = json.dumps(events + observations)
    for value in PRIVATE:
        assert value not in public, "private source content retained"
    assert len({item["observation_id"] for item in observations}) == len(observations), "duplicate observation identity"


def run_case(binary: Path, harness_type: Any, root: Path, case: dict[str, Any], contract: dict[str, Any]) -> dict[str, Any]:
    folder = root / case["id"]
    folder.mkdir()
    harness = None
    try:
        harness = harness_type(binary, folder)
        profile, case_id = case["profile"], case["id"]
        definition = harness.definition(profile)
        observer = definition["observers"][0]
        observer.update(mode="until_terminal", interval_seconds=3600, criterion_revision="C1")
        if "steps" in case:
            observer["interval_seconds"] = 1
        if case_id.startswith("usage-deadline-"):
            observer["deadline_seconds"] = 1
        if case_id == "usage-command-timeout":
            timeout = folder / "timeout.py"
            timeout.write_text(TIMEOUT, encoding="utf-8")
            observer.update(program=sys.executable, args=[str(timeout), str(harness.calls)], timeout_seconds=1,
                            fingerprint_files=[str(timeout)])
        journal, destination = a2a_inputs(case_id, definition) if profile == "a2a" else usage_inputs(case_id, definition)
        journal_path = folder / ("a2a_snapshots.json" if profile == "a2a" else "usage_journal.json")
        destination_path = folder / ("a2a_destination.json" if profile == "a2a" else "usage_resolver.json")
        write(journal_path, journal)
        write(destination_path, destination)
        inputs_before = {path: path.read_bytes() for path in (journal_path, destination_path)}
        harness.save(definition, 0)
        assert harness.calls_so_far() == [], "registration executed source commands"
        harness.cli("observe", "sync")
        if "steps" not in case or not harness.records(definition["id"], "observation"):
            harness.client.call("observe_sync", {})
        expected = dict(contract["defaults"], **contract["profile_contracts"][profile])
        expected.update(case.get("overrides", {}), result=case["result"], reason_code=case["reason_code"])
        source = definition["id"]
        for index, step in enumerate(case.get("steps", [{}])):
            if index:
                ready = fixture("a2a_destination.json" if profile == "a2a" else "usage_resolver.json")
                write(destination_path, ready)
                inputs_before[destination_path] = destination_path.read_bytes()
                time.sleep(1.05)
                harness.client.call("observe_sync", {})
            stage = dict(expected, **step)
            assert_records(harness, source, stage)
        if case_id.startswith("usage-deadline-"):
            time.sleep(1.05)
        status = harness.client.call("observe_status", {})
        assert harness.cli("observe", "status") == status, "CLI status differs from MCP"
        row = next(item for item in status["sources"] if item["source"]["id"] == source)
        for key in ("pending", "overdue"):
            if key in expected:
                assert row[key] == expected[key], "source status differs: " + key
        assert_records(harness, source, expected)
        before = tree_hash(harness.home)
        pages = harness.readonly_queries([source])
        assert tree_hash(harness.home) == before
        assert all(path.read_bytes() == content for path, content in inputs_before.items()), "adapter changed its fixed inputs"
        assert all(call["mode"] in ("reader", "observer") for call in harness.calls_so_far()), "unexpected controller command"
        retained = "\n".join(path.read_text(encoding="utf-8") for path in harness.home.rglob("*") if path.is_file())
        for value in PRIVATE:
            assert value not in retained, "private source content retained on disk"
        return {"id": case_id, "result": case["result"], "reason_code": case["reason_code"], "query_pages": pages,
                "simulated_scheduler_commands": len(scheduler_calls(harness.folder))}
    except AssertionError as error:
        raise AssertionError(case["id"] + ": " + str(error)) from error
    finally:
        if harness is not None:
            harness.client.close()


def run(binary: Path, harness_type: Any) -> dict[str, Any]:
    contract = fixture("profile_expected.json")
    assert contract["schema_version"] == "rindcut.profile.acceptance/v1"
    cases = contract["cases"]
    assert cases and len({case["id"] for case in cases}) == len(cases), "acceptance cases missing or duplicated"
    before = hashlib.sha256(binary.read_bytes()).hexdigest()
    with tempfile.TemporaryDirectory(prefix="acme-profile-matrix-") as folder:
        with ThreadPoolExecutor(max_workers=4) as pool:
            results = list(pool.map(lambda case: run_case(binary, harness_type, Path(folder), case, contract), cases))
    assert hashlib.sha256(binary.read_bytes()).hexdigest() == before, "acceptance binary changed"
    return {"cases": results, "case_count": len(results), "independent_expected": "profile_expected.json",
            "binary_sha256": before, "isolated_home_per_case": True, "cli_json_and_jsonl_equal_mcp": True,
            "readonly_adapter_calls": 0, "readonly_home_hash_unchanged": True,
             "source_inputs_unchanged": True, "private_values_not_retained": True,
             "authority_not_promoted": True, "controller_commands": 0, "live_provider_calls": 0,
             "os_scheduler_isolated": True,
             "simulated_scheduler_commands": sum(case["simulated_scheduler_commands"] for case in results)}
