import argparse
import hashlib
import re
import sys
from typing import Any

import adapter


REVISION = "usage-example-v1"
PROJECTION = "acme.run_usage.snapshot.v1"
SCHEMA = "acme.run_usage/v1"
PARTIAL = "acme.run_usage.partial"
FINAL = "acme.run_usage.final"
OTEL_REVISION = "dc3571562603552dc555c667dfa327a638d94ab2"
MAX_INTEGER = 9007199254740991
OPAQUE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$")
DIMENSIONS = ("input_tokens", "output_tokens", "tool_calls", "elapsed_ms")
UNITS = {"input_tokens": "tokens", "output_tokens": "tokens", "tool_calls": "calls", "elapsed_ms": "milliseconds"}
COUNTERS = DIMENSIONS + ("cached_input_tokens", "reasoning_output_tokens")
SCOPE = ("provider", "api_version", "model", "meter_profile")
ATTRIBUTES = ("budget_id", "budget_revision", "usage_complete") + SCOPE + COUNTERS
CAPABILITY = {"kind": "snapshot", "types": [PARTIAL, FINAL],
              "limitations": ["supplied_immutable_snapshots_only", "owner_supplied_receipts",
                              "not_billing_verification", "no_unrecorded_consumption"]}
PROVIDERS = {"openai": "chat.completions.v1", "anthropic": "messages.2023-06-01"}


class EvidenceError(ValueError):
    pass


def opaque(value: Any) -> str:
    if not isinstance(value, str) or not OPAQUE.fullmatch(value):
        raise EvidenceError("invalid_reference")
    return value


def integer(value: Any) -> int:
    if type(value) is not int or not 0 <= value <= MAX_INTEGER:
        raise EvidenceError("invalid_counter")
    return value


def sha(value: Any) -> str:
    return "sha256:" + hashlib.sha256(adapter.encoded(value)).hexdigest()


def proof(kind: str, namespace: str, identity: str, digest: str) -> dict[str, Any]:
    if not isinstance(digest, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        raise EvidenceError("invalid_digest")
    return {"kind": kind, "ref": {"namespace": namespace, "id": opaque(identity)}, "digest": digest}


def snapshot_event(request: dict[str, Any], generation: str, snapshot: dict[str, Any]) -> dict[str, Any]:
    complete = snapshot["usage_complete"]
    if type(complete) is not bool:
        raise EvidenceError("invalid_completeness")
    result = {"source_instance": request["source_instance"], "generation": generation,
              "stream": request["stream"], "id": opaque(snapshot["snapshot_id"]),
              "type": FINAL if complete else PARTIAL, "schema_version": SCHEMA,
              "adapter_revision": REVISION, "projection_profile": PROJECTION,
              "action_id": opaque(snapshot["run_ref"]), "links": [], "attributes": {}, "evidence": []}
    attributes = result["attributes"]
    attributes["usage_complete"] = complete
    for name in SCOPE + ("budget_id", "budget_revision"):
        attributes[name] = opaque(snapshot[name])
    for name in COUNTERS:
        if name in snapshot["usage"]:
            attributes[name] = integer(snapshot["usage"][name])
    if "attempt_id" in snapshot:
        result["attempt_id"] = opaque(snapshot["attempt_id"])
    adapter.occurrence(result, snapshot.get("occurred_at"))
    result["evidence"] = [
        proof("usage_manifest", "acme.usage.manifest", snapshot["manifest_id"], snapshot["manifest_digest"]),
        proof("budget", "acme.usage.budget", snapshot["budget_id"], snapshot["budget_digest"]),
    ]
    if "replaces" in snapshot:
        previous = snapshot["replaces"]
        if set(previous) != {"snapshot_id", "type", "generation"} or previous["type"] not in (PARTIAL, FINAL):
            raise EvidenceError("invalid_replacement")
        result["links"] = [{"relation": "replaces_snapshot", "target": {
            "source_id": request["source_id"], "source_instance": request["source_instance"],
            "generation": opaque(previous["generation"]), "stream": request["stream"],
            "type": previous["type"], "id": opaque(previous["snapshot_id"]),
            "projection_profile": PROJECTION}}]
    return result


def read_source(request: dict[str, Any], journal: dict[str, Any]) -> dict[str, Any]:
    required = {"protocol", "operation", "source_id", "source_revision", "source_instance", "stream",
                "generation", "cursor", "limit"}
    if set(request) != required or request["protocol"] != adapter.SOURCE_PROTOCOL or request["operation"] != "read":
        raise EvidenceError("invalid_request")
    for name in ("source_id", "source_instance", "stream"):
        opaque(request[name])
    if type(request["source_revision"]) is not int or request["source_revision"] < 1:
        raise EvidenceError("invalid_revision")
    if type(request["limit"]) is not int or not 1 <= request["limit"] <= 100:
        raise EvidenceError("invalid_limit")
    generation = opaque(journal["generation"])
    snapshots = journal["snapshots"]
    if not isinstance(snapshots, list):
        raise EvidenceError("invalid_snapshots")
    result = {name: request[name] for name in ("protocol", "source_id", "source_revision", "source_instance", "stream")}
    result.update(generation=generation, capability=CAPABILITY, status="ok", records=[], next_cursor=None,
                  has_more=False, coverage={"kind": "snapshot", "limitations": CAPABILITY["limitations"]})
    # Cursor continuity covers the published projection rather than discarded private input
    events = [snapshot_event(request, generation, snapshot) for snapshot in snapshots]
    try:
        if request["generation"] not in (None, generation):
            raise EvidenceError("cursor_invalid")
        offset = adapter.cursor_offset(request["cursor"], generation, events)
    except (ValueError, TypeError, KeyError, UnicodeError):
        result["status"] = "cursor_invalid"
        return result
    end = min(offset + request["limit"], len(events))
    result["records"] = events[offset:end]
    result["next_cursor"] = adapter.cursor_for(generation, events, end)
    result["has_more"] = end < len(events)
    return result


def openai_usage(usage: dict[str, Any]) -> dict[str, int]:
    result = {}
    for native, normalized in (("prompt_tokens", "input_tokens"), ("completion_tokens", "output_tokens")):
        if native in usage:
            result[normalized] = integer(usage[native])
    for details, native, normalized, total in (
        ("prompt_tokens_details", "cached_tokens", "cached_input_tokens", "input_tokens"),
        ("completion_tokens_details", "reasoning_tokens", "reasoning_output_tokens", "output_tokens"),
    ):
        if native in usage.get(details, {}):
            result[normalized] = integer(usage[details][native])
            if total not in result or result[normalized] > result[total]:
                raise EvidenceError("invalid_subset")
    if "total_tokens" in usage:
        if any(name not in result for name in ("input_tokens", "output_tokens")):
            raise EvidenceError("total_mismatch")
        if integer(usage["total_tokens"]) != result["input_tokens"] + result["output_tokens"]:
            raise EvidenceError("total_mismatch")
    return result


def anthropic_usage(usage: dict[str, Any]) -> dict[str, int]:
    result = {}
    names = ("input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens")
    if all(name in usage for name in names):
        result["input_tokens"] = integer(sum(integer(usage[name]) for name in names))
        result["cached_input_tokens"] = integer(usage["cache_read_input_tokens"])
    if "output_tokens" in usage:
        result["output_tokens"] = integer(usage["output_tokens"])
    return result


def anthropic_stream(events: list[dict[str, Any]]) -> tuple[dict[str, int], bool]:
    starts = [event for event in events if event["type"] == "message_start"]
    if len(starts) != 1 or events[0] != starts[0]:
        return {}, False
    usage = dict(starts[0]["message"]["usage"])
    deltas = [event for event in events if event["type"] == "message_delta"]
    counters = ("input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "output_tokens")
    for delta in deltas:
        for name in counters:
            if name not in delta.get("usage", {}):
                continue
            latest = integer(delta["usage"][name])
            if name in usage and latest < integer(usage[name]):
                raise EvidenceError("counter_reset")
            usage[name] = latest
    result = anthropic_usage(usage)
    final_usage = bool(deltas) and "output_tokens" in deltas[-1].get("usage", {})
    stopped = events[-1]["type"] == "message_stop" and sum(event["type"] == "message_stop" for event in events) == 1
    failed = any(event["type"] == "error" for event in events)
    dimensions_present = all(name in result for name in ("input_tokens", "output_tokens"))
    complete = final_usage and stopped and not failed and dimensions_present
    return result, complete


def normalize(native: dict[str, Any], provider: str) -> tuple[dict[str, int], bool]:
    kind = native["kind"]
    if provider == "openai" and kind == "openai.chat.completion":
        result = openai_usage(native["usage"])
        return result, all(name in result for name in ("input_tokens", "output_tokens"))
    if provider == "anthropic" and kind == "anthropic.message":
        result = anthropic_usage(native["usage"])
        return result, all(name in result for name in ("input_tokens", "output_tokens"))
    if provider == "openai" and kind == "openai.chat.stream":
        if any(chunk.get("error") for chunk in native["chunks"]):
            return {}, False
        totals = [chunk["usage"] for chunk in native["chunks"] if chunk.get("usage") is not None]
        final = [chunk for chunk in native["chunks"] if chunk.get("usage") is not None and chunk.get("choices") == []]
        if len(totals) != 1 or len(final) != 1 or native["chunks"][-1] != final[0]:
            return {}, False
        result = openai_usage(totals[0])
        return result, all(name in result for name in ("input_tokens", "output_tokens"))
    if provider == "anthropic" and kind == "anthropic.stream":
        return anthropic_stream(native["events"])
    raise EvidenceError("unsupported_native_profile")


def scope_matches(value: dict[str, Any], expected: dict[str, Any]) -> bool:
    return all(value.get(name) == expected.get(name) for name in SCOPE)


def unique_values(values: list[dict[str, Any]], keys: tuple[str, ...]) -> list[dict[str, Any]]:
    result = {}
    for value in values:
        identity = tuple(opaque(value[key]) for key in keys)
        if identity in result and result[identity] != value:
            raise EvidenceError("identity_conflict")
        result[identity] = value
    return list(result.values())


def accounting_roots(
    selected: dict[str, tuple[dict[str, Any], dict[str, int]]],
) -> list[tuple[dict[str, Any], dict[str, int]]]:
    roots = []
    requests = set()
    children = {}
    for receipt, metrics in selected.values():
        request = opaque(receipt["request_ref"])
        if receipt.get("usage_scope") not in ("inclusive", "exclusive"):
            raise EvidenceError("invalid_usage_scope")
        if receipt.get("accounting_plane") not in ("provider", "proxy"):
            raise EvidenceError("invalid_accounting_plane")
        parent, duplicate = receipt.get("included_in"), receipt.get("duplicate_of")
        if parent is not None and duplicate is not None:
            raise EvidenceError("ambiguous_inclusion")
        if duplicate is not None:
            owner = inclusion_owner(selected, duplicate, receipt["id"])
            same_request = receipt["attempt_id"] == owner[0]["attempt_id"] and request == owner[0]["request_ref"]
            if not same_request or metrics != owner[1]:
                raise EvidenceError("duplicate_binding_mismatch")
            continue
        if receipt["accounting_plane"] != "provider":
            raise EvidenceError("unbound_accounting_plane")
        if request in requests:
            raise EvidenceError("duplicate_consumption")
        requests.add(request)
        if parent is None:
            roots.append((receipt, metrics))
            continue
        owner = inclusion_owner(selected, parent, receipt["id"])
        if receipt["usage_scope"] != "exclusive" or owner[0]["usage_scope"] != "inclusive":
            raise EvidenceError("inclusion_scope_unknown")
        children.setdefault(owner[0]["id"], []).append(metrics)
    if not roots:
        raise EvidenceError("empty_receipt_scope")
    for identity, values in children.items():
        parent_metrics = selected[identity][1]
        for name, maximum in parent_metrics.items():
            # Missing optional child counters leave a lower bound rather than an exact zero
            known = sum(value[name] for value in values if name in value)
            if known > maximum:
                raise EvidenceError("child_consumption_exceeds_parent")
    return roots


def inclusion_owner(selected: dict[str, tuple[dict[str, Any], dict[str, int]]], identity: str,
                    child_id: str) -> tuple[dict[str, Any], dict[str, int]]:
    owner = selected.get(opaque(identity))
    if owner is None or owner[0]["id"] == child_id:
        raise EvidenceError("invalid_inclusion")
    if owner[0].get("included_in") is not None or owner[0].get("duplicate_of") is not None:
        raise EvidenceError("inclusion_scope_unknown")
    if owner[0]["accounting_plane"] != "provider":
        raise EvidenceError("invalid_accounting_plane")
    return owner


def aggregate(manifest: dict[str, Any], receipts: list[dict[str, Any]]) -> dict[str, int]:
    if manifest.get("complete") is not True or manifest.get("primary_accounting_plane") != "provider":
        raise EvidenceError("incomplete_manifest")
    if manifest.get("otel_revision") != OTEL_REVISION:
        raise EvidenceError("meter_revision_mismatch")
    refs = unique_values(manifest["receipts"], ("id", "version"))
    available = {(value["id"], value["version"]): value for value in unique_values(receipts, ("id", "version"))}
    selected = {}
    for ref in refs:
        receipt = available.get((ref["id"], ref["version"]))
        if receipt is None:
            raise EvidenceError("receipt_unavailable")
        if sha(receipt) != ref["digest"]:
            raise EvidenceError("receipt_digest_mismatch")
        if not scope_matches(receipt, manifest) or receipt.get("run_ref") != manifest["run_ref"]:
            raise EvidenceError("receipt_scope_mismatch")
        metrics, complete = normalize(receipt["native"], receipt["provider"])
        if not complete:
            raise EvidenceError("receipt_incomplete")
        selected[ref["id"]] = (receipt, metrics)
    if len(selected) != len(refs):
        raise EvidenceError("receipt_version_ambiguous")
    expected_attempts = set(opaque(value) for value in manifest["expected_attempts"])
    actual_attempts = {opaque(value[0]["attempt_id"]) for value in selected.values()}
    repeated_attempt = len(expected_attempts) != len(manifest["expected_attempts"])
    if not expected_attempts or repeated_attempt or actual_attempts != expected_attempts:
        raise EvidenceError("attempt_coverage_mismatch")
    result = {"input_tokens": 0, "output_tokens": 0}
    optional = {"cached_input_tokens", "reasoning_output_tokens"}
    roots = accounting_roots(selected)
    for _, metrics in roots:
        optional.intersection_update(metrics)
    for _, metrics in roots:
        for name in ("input_tokens", "output_tokens") + tuple(sorted(optional)):
            result[name] = integer(result.get(name, 0) + metrics[name])
    calls = unique_values(manifest["tool_calls"], ("id",))
    for call in calls:
        if (call.get("run_ref") != manifest["run_ref"]
                or call.get("scope") not in ("direct_client", "subagent", "provider_internal")
                or call.get("status") not in ("success", "failed")):
            raise EvidenceError("invalid_tool_scope")
    result["tool_calls"] = integer(sum(call["scope"] == "direct_client" for call in calls))
    start, end = manifest["clock"]["start"], manifest["clock"]["end"]
    if opaque(start["clock"]) != opaque(end["clock"]) or integer(end["ms"]) < integer(start["ms"]):
        raise EvidenceError("invalid_clock")
    result["elapsed_ms"] = integer(end["ms"] - start["ms"])
    return result


def observe(request: dict[str, Any], resolver: dict[str, Any]) -> dict[str, Any]:
    names = ("protocol", "source_id", "source_revision", "observer_id", "observer_revision", "criterion_id",
             "criterion_revision", "series_id", "target_ref")
    if request.get("protocol") != adapter.OBSERVER_PROTOCOL:
        raise EvidenceError("invalid_protocol")
    response = {name: request[name] for name in names}
    response.update(result="unknown", reason_code="invalid_usage_evidence", evidence=[])
    try:
        event, parameters = request["event"], request["parameters"]
        attrs = event["attributes"]
        if event["type"] != FINAL or attrs.get("usage_complete") is not True:
            raise EvidenceError("partial_record_only")
        if event["schema_version"] != SCHEMA or event["projection_profile"] != PROJECTION:
            raise EvidenceError("unsupported_projection")
        for name in SCOPE + ("budget_id", "budget_revision"):
            if attrs[name] != parameters["expected_" + name]:
                raise EvidenceError("policy_scope_mismatch")
        if attrs["provider"] not in PROVIDERS or attrs["api_version"] != PROVIDERS[attrs["provider"]]:
            raise EvidenceError("unsupported_api_version")
        evidence = event["evidence"]
        budget_proofs = [value for value in evidence if value["kind"] == "budget"
                         and value["ref"]["namespace"] == "acme.usage.budget"]
        manifest_proofs = [value for value in evidence if value["kind"] == "usage_manifest"
                           and value["ref"]["namespace"] == "acme.usage.manifest"]
        if len(budget_proofs) != 1 or len(manifest_proofs) != 1:
            raise EvidenceError("evidence_binding_missing")
        budget_proof, manifest_proof = budget_proofs[0], manifest_proofs[0]
        budgets = [value for value in resolver["budgets"] if value.get("id") == attrs["budget_id"]
                   and value.get("revision") == attrs["budget_revision"]]
        if len(budgets) != 1:
            raise EvidenceError("budget_version_unavailable")
        budget = budgets[0]
        budget_digest = sha(budget)
        if (budget_proof["ref"]["id"] != budget["id"] or budget_proof["digest"] != budget_digest
                or budget_digest != parameters["expected_budget_digest"]):
            raise EvidenceError("budget_digest_mismatch")
        if (not scope_matches(budget, attrs) or budget.get("comparison") != "inclusive"
                or budget.get("missing") != "unknown"):
            raise EvidenceError("budget_contract_mismatch")
        limits = budget["limits"]
        if not limits or not set(limits).issubset(DIMENSIONS):
            raise EvidenceError("budget_dimensions_invalid")
        for value in limits.values():
            integer(value)
        if budget.get("units") != {name: UNITS[name] for name in limits}:
            raise EvidenceError("budget_units_invalid")
        manifests = [value for value in resolver["manifests"] if value.get("id") == manifest_proof["ref"]["id"]]
        if not manifests:
            binding = {"id": manifest_proof["ref"]["id"], "digest": manifest_proof["digest"],
                       "snapshot_id": event["id"], "run_ref": event["action_id"],
                       "budget_id": attrs["budget_id"], "budget_revision": attrs["budget_revision"]}
            binding.update({name: attrs[name] for name in SCOPE})
            if binding in resolver.get("preparing", []):
                response.update(result="pending", reason_code="fixed_receipt_preparing",
                                evidence=[manifest_proof, budget_proof])
                return response
            raise EvidenceError("manifest_unavailable")
        if len(manifests) != 1 or sha(manifests[0]) != manifest_proof["digest"]:
            raise EvidenceError("manifest_digest_mismatch")
        manifest = manifests[0]
        binding = {"snapshot_id": event["id"], "run_ref": event["action_id"],
                   "budget_id": attrs["budget_id"], "budget_revision": attrs["budget_revision"]}
        if not scope_matches(manifest, attrs) or any(manifest.get(key) != value for key, value in binding.items()):
            raise EvidenceError("manifest_scope_mismatch")
        if "attempt_id" in event and manifest.get("expected_attempts") != [event["attempt_id"]]:
            raise EvidenceError("attempt_binding_mismatch")
        metrics = aggregate(manifest, resolver["receipts"])
        for name in COUNTERS:
            if name in attrs:
                integer(attrs[name])
        missing_or_different = any(name not in attrs or attrs[name] != metrics[name] for name in metrics)
        unexpected = any(name in attrs and name not in metrics for name in COUNTERS)
        if missing_or_different or unexpected:
            raise EvidenceError("projection_mismatch")
        if any(name not in metrics for name in limits):
            raise EvidenceError("required_dimension_missing")
        exceeded = any(integer(metrics[name]) > maximum for name, maximum in limits.items())
        response.update(result="refuted" if exceeded else "confirmed",
                        reason_code="budget_exceeded" if exceeded else "within_budget",
                        evidence=[manifest_proof, budget_proof])
    except EvidenceError as error:
        response["reason_code"] = str(error)
        return response
    except (ValueError, KeyError, TypeError, AttributeError, IndexError, OverflowError):
        return response
    return response


def select_snapshot(events: list[dict[str, Any]]) -> dict[str, Any]:
    for value in events:
        opaque(value.get("source_id"))
    unique = unique_values(events, ("generation", "type", "id"))
    if not unique:
        raise EvidenceError("snapshot_missing")
    first = unique[0]
    scope = SCOPE + ("budget_id", "budget_revision")
    identity_scope = ("source_id", "source_instance", "generation", "stream", "projection_profile", "action_id")
    mixed_identity = any(any(value[key] != first[key] for key in identity_scope) for value in unique)
    mixed_policy = any(any(value["attributes"][key] != first["attributes"][key] for key in scope) for value in unique)
    if mixed_identity or mixed_policy:
        raise EvidenceError("snapshot_scope_ambiguous")
    identities = {(value["generation"], value["type"], value["id"]): value for value in unique}
    replaced = set()
    previous = {}
    for value in unique:
        own = (value["generation"], value["type"], value["id"])
        for link in value["links"]:
            if link["relation"] != "replaces_snapshot":
                continue
            target = link["target"]
            identity = (target["generation"], target["type"], target["id"])
            mismatched_source = any(target[key] != value[key] for key in
                                    ("source_id", "source_instance", "stream", "projection_profile"))
            if (identity not in identities or own in previous or identity in replaced
                    or mismatched_source or identity == own):
                raise EvidenceError("replacement_unresolved")
            replaced.add(identity)
            previous[own] = identity
    heads = [value for key, value in identities.items() if key not in replaced]
    if len(heads) != 1 or heads[0]["type"] != FINAL:
        raise EvidenceError("snapshot_incomplete_or_ambiguous")
    current = (heads[0]["generation"], heads[0]["type"], heads[0]["id"])
    visited = set()
    while current not in visited:
        visited.add(current)
        if current not in previous:
            break
        current = previous[current]
    if len(visited) != len(identities):
        raise EvidenceError("replacement_unresolved")
    return heads[0]


def main() -> int:
    parser = argparse.ArgumentParser(description="Offline usage snapshots and fixed-budget evidence comparison")
    parser.add_argument("mode", choices=("reader", "observer"))
    parser.add_argument("--input", required=True, help="owner-supplied journal or separate receipt and budget resolver")
    args = parser.parse_args()
    try:
        raw = sys.stdin.buffer.read(adapter.LIMIT + 1)
        if len(raw) > adapter.LIMIT:
            raise EvidenceError("input_limit")
        request = adapter.decode(raw)
        data = adapter.load(args.input)
        result = read_source(request, data) if args.mode == "reader" else observe(request, data)
        output = adapter.encoded(result)
        if len(output) > adapter.LIMIT:
            raise EvidenceError("output_limit")
        sys.stdout.buffer.write(output + b"\n")
    except (OSError, ValueError, KeyError, TypeError, UnicodeError, AttributeError,
            IndexError, RecursionError, OverflowError):
        sys.stderr.write("usage_adapter_failed\n")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
