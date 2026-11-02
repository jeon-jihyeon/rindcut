import argparse
import base64
import datetime
import hashlib
import re
import sys
from typing import Any

import adapter


REVISION = "a2a-example-v1"
PROJECTION = "a2a.task.snapshot.v1"
SCHEMA = "a2a.1.0.1"
EVENT_TYPE = "a2a.task.snapshot"
CAPABILITY = {"kind": "snapshot", "types": [EVENT_TYPE],
              "limitations": ["immutable_supplied_samples_only", "metadata_only", "no_intermediate_attempts",
                              "no_missing_chunk_reconstruction", "no_protocol_conformance_claim"]}
ATTRIBUTES = ["scope_ref", "wire_version", "context_ref", "task_state", "artifact_state", "chunks_complete"]
TOKEN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$")
DIGEST = re.compile(r"^sha256:[0-9a-f]{64}$")
STATES = {"TASK_STATE_" + value for value in ("UNSPECIFIED", "SUBMITTED", "WORKING", "COMPLETED", "FAILED",
                                            "CANCELED", "INPUT_REQUIRED", "REJECTED", "AUTH_REQUIRED")}
EVENT_FIELDS = ("source_instance", "generation", "stream", "id", "type", "schema_version", "adapter_revision",
                "projection_profile", "occurred_at", "action_id", "attempt_id", "links", "attributes")


def fields(value: Any, required: set[str], optional: set[str] = frozenset()) -> None:
    if not isinstance(value, dict) or not required <= value.keys() or value.keys() - required - optional:
        raise ValueError("invalid_fields")


def token(value: Any) -> str:
    if not isinstance(value, str) or not TOKEN.fullmatch(value):
        raise ValueError("invalid_token")
    return value


def hash_ref(value: Any) -> str:
    return hashlib.sha256(adapter.encoded(value)).hexdigest()


def scope_ref(scope: Any) -> str:
    fields(scope, {"server", "tenant", "caller"})
    for value in scope.values():
        token(value)
    return hash_ref(scope)


def timestamp(value: Any) -> int:
    if not isinstance(value, str) or not adapter.TIME.fullmatch(value):
        raise ValueError("invalid_time")
    parsed = datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
    if value[-1] != "Z" and (int(value[-5:-3]) > 23 or int(value[-2:]) > 59):
        raise ValueError("invalid_time")
    fraction = re.search(r"\.(\d+)", value)
    nanoseconds = int(fraction[1].ljust(9, "0")) if fraction else 0
    elapsed = parsed.replace(microsecond=0) - datetime.datetime(1970, 1, 1, tzinfo=datetime.timezone.utc)
    return (elapsed.days * 86400 + elapsed.seconds) * 1000000000 + nanoseconds


def canonical_time(value: str) -> str:
    seconds, nanos = divmod(timestamp(value), 1000000000)
    parsed = datetime.datetime(1970, 1, 1, tzinfo=datetime.timezone.utc) + datetime.timedelta(seconds=seconds)
    return (f"{parsed.year:04}-{parsed.month:02}-{parsed.day:02}T{parsed.hour:02}:{parsed.minute:02}:"
            f"{parsed.second:02}.{nanos:09}Z")


def proof(kind: str, namespace: str, reference: str, digest: str) -> dict[str, Any]:
    if not isinstance(digest, str) or not DIGEST.fullmatch(digest):
        raise ValueError("invalid_digest")
    return {"kind": kind, "ref": {"namespace": namespace, "id": reference}, "digest": digest}


def snapshot_digest(event: dict[str, Any]) -> str:
    projection = {name: event[name] for name in EVENT_FIELDS if name in event}
    projection["evidence"] = [item for item in event["evidence"] if item.get("kind") == "artifact_version"]
    return adapter.digest(projection)


def project(request: dict[str, Any], generation: str, scope: str, sample: Any) -> dict[str, Any]:
    fields(sample, {"sample_id", "task", "chunks_complete"}, {"artifact", "sampled_at", "attempt_id", "links"})
    sample_id = token(sample["sample_id"])
    task = sample["task"]
    if not isinstance(task, dict) or not isinstance(task.get("status"), dict):
        raise ValueError("invalid_task")
    native_task = adapter.identifier(task["id"])
    state = task["status"]["state"]
    if not isinstance(state, str) or state not in STATES or type(sample["chunks_complete"]) is not bool:
        raise ValueError("invalid_task_state")
    artifacts = task.get("artifacts", [])
    if not isinstance(artifacts, list) or len(artifacts) > 100:
        raise ValueError("invalid_artifacts")
    attributes = {"scope_ref": scope, "wire_version": "1.0", "task_state": state,
                  "artifact_state": "present" if artifacts else "absent",
                  "chunks_complete": sample["chunks_complete"]}
    if "contextId" in task:
        if not isinstance(task["contextId"], str):
            raise ValueError("invalid_context")
        if task["contextId"]:
            attributes["context_ref"] = hash_ref([scope, adapter.identifier(task["contextId"])])
    result = {"source_instance": request["source_instance"], "generation": generation, "stream": request["stream"],
              "id": sample_id, "type": EVENT_TYPE, "schema_version": SCHEMA, "adapter_revision": REVISION,
              "projection_profile": PROJECTION, "action_id": hash_ref([scope, native_task]), "links": [],
              "attributes": attributes, "evidence": []}
    if "attempt_id" in sample:
        result["attempt_id"] = hash_ref([scope, native_task, adapter.identifier(sample["attempt_id"])])
    if "sampled_at" in sample:
        result["occurred_at"] = canonical_time(sample["sampled_at"])
    links = sample.get("links", [])
    if not isinstance(links, list) or len(links) > 32:
        raise ValueError("invalid_links")
    for link in links:
        fields(link, {"relation", "sample_id"})
        if link["relation"] not in ("replaces", "caused_by", "retry_of", "corrects"):
            raise ValueError("invalid_relation")
        result["links"].append({"relation": link["relation"], "target": {
            "source_id": request["source_id"], "source_instance": request["source_instance"],
            "generation": generation, "stream": request["stream"], "type": EVENT_TYPE,
            "id": token(link["sample_id"]), "projection_profile": PROJECTION}})
    if "artifact" in sample:
        artifact = sample["artifact"]
        fields(artifact, {"artifact_id", "version", "digest"})
        artifact_id = adapter.identifier(artifact["artifact_id"])
        if not any(isinstance(item, dict) and item.get("artifactId") == artifact_id for item in artifacts):
            raise ValueError("artifact_binding_missing")
        artifact_ref = hash_ref([scope, native_task, artifact_id, token(artifact["version"])])
        result["evidence"].append(proof("artifact_version", "a2a.artifact", artifact_ref, artifact["digest"]))
    result["evidence"].append(proof("source_snapshot", "a2a.snapshot", hash_ref([scope, sample_id]),
                                    snapshot_digest(result)))
    return result


def read_source(request: dict[str, Any], journal: dict[str, Any]) -> dict[str, Any]:
    fields(request, {"protocol", "operation", "source_id", "source_revision", "source_instance", "stream",
                     "generation", "cursor", "limit"})
    if request["protocol"] != adapter.SOURCE_PROTOCOL or request["operation"] != "read":
        raise ValueError("invalid_request")
    if type(request["source_revision"]) is not int or request["source_revision"] < 1:
        raise ValueError("invalid_revision")
    if type(request["limit"]) is not int or not 1 <= request["limit"] <= 100:
        raise ValueError("invalid_limit")
    for name in ("source_id", "source_instance", "stream"):
        token(request[name])
    fields(journal, {"profile", "wire_version", "scope", "source_instance", "generation", "stream", "samples"})
    if journal["profile"] != PROJECTION or journal["wire_version"] != "1.0":
        raise ValueError("unsupported_profile")
    if any(request[name] != journal[name] for name in ("source_instance", "stream")):
        raise ValueError("scope_mismatch")
    generation = token(journal["generation"])
    scope = scope_ref(journal["scope"])
    if not isinstance(journal["samples"], list) or len(journal["samples"]) > 10000:
        raise ValueError("invalid_samples")
    records = [project(request, generation, scope, sample) for sample in journal["samples"]]
    result = {name: request[name] for name in ("protocol", "source_id", "source_revision", "source_instance", "stream")}
    result.update(generation=generation, capability=CAPABILITY, status="ok", records=[], next_cursor=None,
                  has_more=False, coverage={"kind": "snapshot", "limitations": CAPABILITY["limitations"]})
    try:
        if request["generation"] not in (None, generation):
            raise ValueError("cursor_invalid")
        offset = adapter.cursor_offset(request["cursor"], generation, records)
    except (ValueError, TypeError, KeyError, UnicodeError):
        result["status"] = "cursor_invalid"
        return result
    end = min(offset + request["limit"], len(records))
    result.update(records=records[offset:end], next_cursor=adapter.cursor_for(generation, records, end),
                  has_more=end < len(records))
    return result


def decision(event: dict[str, Any], parameters: dict[str, Any], destination: dict[str, Any],
             now: int) -> tuple[str, str, list[dict[str, Any]]]:
    fields(parameters, {"expected_scope_ref", "expected_state", "require_artifact", "byte_equality",
                        "expected_window_start", "expected_window_end"},
           {"cancel_cutoff", "idempotency_scope", "require_effect"})
    for name in ("require_artifact", "byte_equality"):
        if type(parameters[name]) is not bool:
            raise ValueError("invalid_criterion")
    if type(parameters.get("require_effect", True)) is not bool:
        raise ValueError("invalid_criterion")
    if not isinstance(parameters["expected_scope_ref"], str) or not re.fullmatch(r"[0-9a-f]{64}", parameters["expected_scope_ref"]):
        raise ValueError("invalid_scope_ref")
    if "cancel_cutoff" in parameters:
        timestamp(parameters["cancel_cutoff"])
    if "idempotency_scope" in parameters:
        token(parameters["idempotency_scope"])
    expected_start, expected_end = timestamp(parameters["expected_window_start"]), timestamp(parameters["expected_window_end"])
    if expected_start > expected_end:
        raise ValueError("invalid_criterion_window")
    token(parameters["expected_state"])
    fields(destination, {"profile", "scope", "readable", "tasks", "artifacts"}, {"correct_previous"})
    if destination["profile"] != PROJECTION or type(destination["readable"]) is not bool:
        raise ValueError("invalid_destination")
    if not isinstance(destination["tasks"], dict) or not isinstance(destination["artifacts"], dict):
        raise ValueError("invalid_destination")
    attributes = event["attributes"]
    if (event["type"] != EVENT_TYPE or event["schema_version"] != SCHEMA or event["projection_profile"] != PROJECTION
            or event["adapter_revision"] != REVISION or attributes.get("wire_version") != "1.0"):
        return "unknown", "profile_mismatch", []
    if (attributes.get("scope_ref") != parameters["expected_scope_ref"]
            or scope_ref(destination["scope"]) != parameters["expected_scope_ref"]):
        return "unknown", "scope_mismatch", []
    source_proofs = [item for item in event["evidence"] if item.get("kind") == "source_snapshot"]
    expected_ref = {"namespace": "a2a.snapshot", "id": hash_ref([attributes["scope_ref"], event["id"]])}
    if (len(source_proofs) != 1 or source_proofs[0].get("ref") != expected_ref
            or source_proofs[0].get("digest") != snapshot_digest(event)):
        return "unknown", "snapshot_binding_mismatch", []
    if attributes.get("chunks_complete") is not True:
        return "unknown", "chunk_gap", []
    if not destination["readable"]:
        return "unknown", "destination_unreadable", []
    actual = destination["tasks"].get(event["action_id"])
    if actual is None:
        return "unknown", "coverage_missing", []
    fields(actual, {"task_ref", "scope_ref", "window_start", "window_end", "complete", "effects", "artifact_state"})
    if actual["task_ref"] != event["action_id"] or actual["scope_ref"] != parameters["expected_scope_ref"]:
        return "unknown", "receipt_binding_mismatch", []
    start, end = timestamp(actual["window_start"]), timestamp(actual["window_end"])
    if start > end or type(actual["complete"]) is not bool or not isinstance(actual["effects"], list):
        raise ValueError("invalid_coverage")
    if len(actual["effects"]) > 1000:
        raise ValueError("effect_limit")
    if actual["artifact_state"] not in ("present", "absent", "unknown"):
        raise ValueError("invalid_artifact_state")
    evidence = [proof("destination_lookup", "a2a.destination", event["action_id"], adapter.digest(actual))]
    if start != expected_start or end != expected_end:
        return "unknown", "coverage_scope_mismatch", evidence
    if actual["complete"] and end > now:
        return "unknown", "coverage_not_closed", evidence
    if not actual["effects"]:
        if actual["complete"] and parameters.get("require_effect", True):
            return "refuted", "required_effect_missing", evidence
        if not actual["complete"]:
            return "pending", "effect_not_yet_observed", evidence
    effect_ids = set()
    for effect in actual["effects"]:
        fields(effect, {"id", "state"}, {"occurred_at", "idempotency_scope"})
        token(effect["id"])
        token(effect["state"])
        if effect["id"] in effect_ids:
            return "unknown", "duplicate_receipt_identity", evidence
        effect_ids.add(effect["id"])
        if "occurred_at" in effect and not start <= timestamp(effect["occurred_at"]) <= end:
            return "unknown", "effect_outside_coverage", evidence
    if "idempotency_scope" in parameters:
        scope = token(parameters["idempotency_scope"])
        if any(effect.get("idempotency_scope") != scope for effect in actual["effects"]):
            return "unknown", "idempotency_scope_unknown", evidence
        if len(actual["effects"]) > 1:
            return "refuted", "duplicate_business_effect", evidence
    if attributes.get("task_state") == "TASK_STATE_CANCELED":
        if "cancel_cutoff" not in parameters or any("occurred_at" not in effect for effect in actual["effects"]):
            return "unknown", "cancel_order_unknown", evidence
        cutoff = timestamp(parameters["cancel_cutoff"])
        if any(timestamp(effect["occurred_at"]) >= cutoff for effect in actual["effects"]):
            return "refuted", "effect_after_cancel_cutoff", evidence
    if not actual["complete"]:
        return "pending", "coverage_open", evidence
    if any(effect["state"] != parameters["expected_state"] for effect in actual["effects"]):
        return "refuted", "effect_mismatch", evidence
    artifact_proofs = [item for item in event["evidence"] if item.get("kind") == "artifact_version"]
    if parameters["require_artifact"] and actual["artifact_state"] != "present":
        if actual["artifact_state"] == "absent":
            return "refuted", "required_artifact_missing", evidence
        return "unknown", "artifact_presence_unknown", evidence
    if parameters["require_artifact"] or parameters["byte_equality"]:
        if not artifact_proofs:
            if actual["artifact_state"] == "absent":
                return "refuted", "required_artifact_missing", evidence
            return "unknown", "artifact_version_missing", evidence
        if len(artifact_proofs) != 1:
            return "unknown", "artifact_version_ambiguous", evidence
        expected = artifact_proofs[0]
        if expected.get("ref", {}).get("namespace") != "a2a.artifact" or not DIGEST.fullmatch(expected.get("digest", "")):
            return "unknown", "artifact_binding_mismatch", evidence
        artifact = destination["artifacts"].get(expected["ref"]["id"])
        if artifact is None:
            return "unknown", "artifact_version_unavailable", evidence
        fields(artifact, {"ref", "readable", "content_base64"})
        if artifact["ref"] != expected["ref"] or artifact["readable"] is not True:
            return "unknown", "artifact_version_unavailable", evidence
        content = base64.b64decode(artifact["content_base64"], validate=True)
        observed = "sha256:" + hashlib.sha256(content).hexdigest()
        evidence.append(proof("artifact_lookup", "a2a.artifact", expected["ref"]["id"], observed))
        if parameters["byte_equality"] and observed != expected["digest"]:
            return "refuted", "artifact_digest_mismatch", evidence
    return "confirmed", "criterion_satisfied", evidence


def observe(request: dict[str, Any], destination: dict[str, Any]) -> dict[str, Any]:
    names = ("protocol", "source_id", "source_revision", "observer_id", "observer_revision", "criterion_id",
             "criterion_revision", "series_id", "target_ref")
    fields(request, set(names) | {"event", "parameters", "now"}, {"previous_observation_id"})
    if request["protocol"] != adapter.OBSERVER_PROTOCOL:
        raise ValueError("invalid_protocol")
    result = {name: request[name] for name in names}
    state, reason, evidence = decision(request["event"], request["parameters"], destination, timestamp(request["now"]))
    result.update(result=state, reason_code=reason, evidence=evidence)
    correction = destination.get("correct_previous", False)
    if type(correction) is not bool:
        raise ValueError("invalid_correction")
    if correction:
        previous = request.get("previous_observation_id")
        if not previous:
            raise ValueError("previous_observation_missing")
        result["supersedes"] = {"store_id": request["target_ref"]["store_id"],
                                "observation_id": adapter.identifier(previous)}
    return result


def main() -> int:
    parser = argparse.ArgumentParser(description="Offline A2A 1.0 immutable Task samples and destination checks")
    parser.add_argument("mode", choices=("reader", "observer"))
    parser.add_argument("--input", required=True, help="owner-supplied snapshot journal or separate destination fixture")
    args = parser.parse_args()
    try:
        wire = sys.stdin.buffer.read(adapter.LIMIT + 1)
        if len(wire) > adapter.LIMIT:
            raise ValueError("input_limit")
        request = adapter.decode(wire)
        source = adapter.load(args.input)
        response = read_source(request, source) if args.mode == "reader" else observe(request, source)
        output = adapter.encoded(response)
        if len(output) > adapter.LIMIT:
            raise ValueError("output_limit")
        sys.stdout.buffer.write(output + b"\n")
    except (OSError, ValueError, KeyError, TypeError, UnicodeError, AttributeError,
            IndexError, RecursionError, OverflowError):
        sys.stderr.write("a2a_adapter_failed\n")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
