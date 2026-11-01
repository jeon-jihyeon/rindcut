import argparse
import base64
import datetime
import hashlib
import json
from pathlib import Path
import re
import sys
import unicodedata
from typing import Any, NoReturn


SOURCE_PROTOCOL = "rindcut.source/v1"
OBSERVER_PROTOCOL = "rindcut.observer/v1"
REVISION = "example-v1"
PROJECTION = "metadata-v1"
LIMIT = 1 << 20
IDENTIFIER = re.compile(r"^[^\x00-\x1f\x7f]{1,256}$")
TIME = re.compile(r"^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?(?:Z|[+-]\d\d:\d\d)$")
CAPABILITIES = {
    "acme": {"kind": "event_stream", "types": [], "limitations": ["fixture_journal_only"]},
    "nodloop": {
        "kind": "event_stream", "types": [],
        "limitations": ["supplied_public_export_only", "envelope_projection_only", "metadata_only",
                         "guard_not_supported", "no_unrecorded_actions"],
    },
    "meetproxy": {
        "kind": "snapshot", "types": [],
        "limitations": ["supplied_posts_export_only", "snapshot_only", "no_intermediate_attempts",
                        "no_destination_readback"],
    },
}


def unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate_key")
        result[key] = value
    return result


def reject_number(value: str) -> NoReturn:
    raise ValueError("invalid_number")


def decode(data: bytes) -> Any:
    return json.loads(data.decode("utf-8"), object_pairs_hook=unique_object, parse_constant=reject_number)


def encoded(value: Any) -> bytes:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False).encode("utf-8")


def digest(value: Any) -> str:
    return "sha256:" + hashlib.sha256(encoded(value)).hexdigest()


def identifier(value: Any) -> str:
    if not isinstance(value, str) or not IDENTIFIER.fullmatch(value) or len(value.encode("utf-8")) > 256:
        raise ValueError("invalid_identifier")
    if any(unicodedata.category(character) == "Cc" for character in value):
        raise ValueError("invalid_identifier")
    return value


def load(path: str) -> dict[str, Any]:
    with Path(path).open("rb") as source:
        data = source.read(8 * LIMIT + 1)
    if len(data) > 8 * LIMIT:
        raise ValueError("input_limit")
    result = decode(data)
    if not isinstance(result, dict):
        raise ValueError("invalid_object")
    return result


def reference(namespace: str, native: Any) -> dict[str, Any]:
    return {"kind": "source_record", "ref": {"namespace": namespace, "id": hashlib.sha256(encoded(native)).hexdigest()}}


def event(request: dict[str, Any], generation: str, native_id: str, event_type: str, schema: str) -> dict[str, Any]:
    return {
        "source_instance": request["source_instance"], "generation": generation, "stream": request["stream"],
        "id": identifier(native_id), "type": identifier(event_type), "schema_version": schema,
        "adapter_revision": REVISION, "projection_profile": PROJECTION, "links": [], "attributes": {}, "evidence": [],
    }


def occurrence(result: dict[str, Any], value: Any) -> None:
    if value is None:
        return
    if not isinstance(value, str) or not TIME.fullmatch(value):
        raise ValueError("invalid_time")
    datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
    result["occurred_at"] = value


def metadata(result: dict[str, Any], key: str, value: Any) -> None:
    if value is None:
        return
    if isinstance(value, str) and len(value.encode("utf-8")) <= 2048:
        result["attributes"][key] = value
        return
    if isinstance(value, bool) or type(value) is int and abs(value) <= 9007199254740991:
        result["attributes"][key] = value
        return
    raise ValueError("invalid_metadata")


def acme_event(request: dict[str, Any], generation: str, record: dict[str, Any]) -> dict[str, Any]:
    result = event(request, generation, record["id"], record["type"], "acme.example/v1")
    result["action_id"] = identifier(record["action_id"])
    occurrence(result, record.get("occurred_at"))
    for name in ("claim", "authority", "verified"):
        metadata(result, "declared_" + name, record.get(name))
    result["evidence"] = [reference("acme.journal", record["id"])]
    return result


def nodloop_event(request: dict[str, Any], generation: str, record: dict[str, Any]) -> dict[str, Any]:
    if type(record.get("schema_version")) is not int or record["schema_version"] != 1:
        raise ValueError("unsupported_schema")
    kind = identifier(record["record_kind"])
    if kind == "guard":
        raise ValueError("guard_not_in_public_transport")
    result = event(request, generation, record["record_id"], "nodloop." + kind, "nodloop.raw/v1")
    occurrence(result, record.get("time"))
    metadata(result, "record_kind", kind)
    metadata(result, "native_schema_version", 1)
    for name in ("verdict", "actor", "authority", "proof"):
        metadata(result, "declared_" + name, record.get(name))
    if record.get("trace_id"):
        result["action_id"] = identifier(record["trace_id"])
    result["evidence"] = [reference("nodloop.records", record["record_id"])]
    return result


def meetproxy_events(request: dict[str, Any], generation: str, sample: dict[str, Any]) -> list[dict[str, Any]]:
    sample_id = identifier(sample["sample_id"])
    posts = sample["posts"]
    if not isinstance(posts, list):
        raise ValueError("invalid_posts")
    results = []
    for post in posts:
        native = {name: post.get(name, "") for name in ("request", "relay", "reply")}
        if not any(native.values()):
            raise ValueError("post_identity_missing")
        native_id = hashlib.sha256(encoded([sample_id, native])).hexdigest()
        result = event(request, generation, native_id, "meetproxy.post.snapshot", "meetproxy.posts.sample/v1")
        occurrence(result, sample.get("sampled_at"))
        metadata(result, "sample_id", sample_id)
        metadata(result, "post_present", True)
        metadata(result, "retracted", bool(post.get("retracted_at")))
        result["evidence"] = [reference("meetproxy.posts", native)]
        results.append(result)
    return results


def cursor_for(generation: str, records: list[dict[str, Any]], offset: int) -> str:
    value = {"generation": generation, "offset": offset, "prefix": digest(records[:offset])}
    return base64.urlsafe_b64encode(encoded(value)).decode("ascii")


def cursor_offset(cursor: str | None, generation: str, records: list[dict[str, Any]]) -> int:
    if cursor is None:
        return 0
    value = decode(base64.b64decode(cursor, altchars=b"-_", validate=True))
    if set(value) != {"generation", "offset", "prefix"} or value["generation"] != generation:
        raise ValueError("cursor_invalid")
    offset = value["offset"]
    if type(offset) is not int or offset < 0 or offset > len(records) or value["prefix"] != digest(records[:offset]):
        raise ValueError("cursor_invalid")
    return offset


def read_source(request: dict[str, Any], journal: dict[str, Any], source_format: str) -> dict[str, Any]:
    required = {"protocol", "operation", "source_id", "source_revision", "source_instance", "stream",
                "generation", "cursor", "limit"}
    if set(request) != required or request["protocol"] != SOURCE_PROTOCOL or request["operation"] != "read":
        raise ValueError("invalid_request")
    if type(request["limit"]) is not int or not 1 <= request["limit"] <= 100:
        raise ValueError("invalid_limit")
    if type(request["source_revision"]) is not int or request["source_revision"] < 1:
        raise ValueError("invalid_revision")
    for name in ("source_id", "source_instance", "stream"):
        identifier(request[name])
    generation = identifier(journal["generation"])
    records = journal["records"]
    if not isinstance(records, list):
        raise ValueError("invalid_records")
    if source_format == "meetproxy":
        records = [item for sample in records for item in meetproxy_events(request, generation, sample)]
    capability = CAPABILITIES[source_format]
    result = {name: request[name] for name in ("protocol", "source_id", "source_revision", "source_instance", "stream")}
    result.update(generation=generation, capability=capability, status="ok", records=[], next_cursor=None,
                  has_more=False,
                  coverage={"kind": capability["kind"], "limitations": capability["limitations"]})
    try:
        if request["generation"] not in (None, generation):
            raise ValueError("cursor_invalid")
        offset = cursor_offset(request["cursor"], generation, records)
    except (ValueError, TypeError, KeyError, UnicodeError):
        result["status"] = "cursor_invalid"
        return result
    end = min(offset + request["limit"], len(records))
    if source_format == "meetproxy":
        result["records"] = records[offset:end]
    else:
        convert = acme_event if source_format == "acme" else nodloop_event
        result["records"] = [convert(request, generation, item) for item in records[offset:end]]
    result["next_cursor"] = cursor_for(generation, records, end)
    result["has_more"] = end < len(records)
    return result


def observe(request: dict[str, Any], destination: dict[str, Any]) -> dict[str, Any]:
    if request.get("protocol") != OBSERVER_PROTOCOL:
        raise ValueError("invalid_protocol")
    names = ("protocol", "source_id", "source_revision", "observer_id", "observer_revision", "criterion_id",
             "criterion_revision", "series_id", "target_ref")
    result = {name: request[name] for name in names}
    action_id = identifier(request["event"]["action_id"])
    actual = destination["effects"].get(action_id)
    expected = request["parameters"]["expected_state"]
    state = "pending" if actual is None else "confirmed" if actual["state"] == expected else "refuted"
    reason = {"pending": "effect_absent", "confirmed": "effect_present", "refuted": "effect_mismatch"}[state]
    proof = reference("acme.destination", action_id)
    proof["kind"] = "destination_lookup"
    proof["digest"] = digest(actual)
    result.update(result=state, reason_code=reason, evidence=[proof])
    correction = destination.get("correct_previous", False)
    if not isinstance(correction, bool):
        raise ValueError("invalid_correction")
    if correction:
        previous = request.get("previous_observation_id")
        if not previous:
            raise ValueError("previous_observation_missing")
        result["supersedes"] = {"store_id": request["target_ref"]["store_id"], "observation_id": previous}
    return result


def main() -> int:
    parser = argparse.ArgumentParser(description="Offline examples of the rindcut source and observer protocols")
    parser.add_argument("mode", choices=("reader", "observer"))
    parser.add_argument("--format", choices=tuple(CAPABILITIES), default="acme")
    parser.add_argument("--input", required=True, help="owner-supplied public export or separate destination fixture")
    args = parser.parse_args()
    try:
        data = sys.stdin.buffer.read(LIMIT + 1)
        if len(data) > LIMIT:
            raise ValueError("input_limit")
        request = decode(data)
        if not isinstance(request, dict):
            raise ValueError("invalid_request")
        source = load(args.input)
        response = read_source(request, source, args.format) if args.mode == "reader" else observe(request, source)
        output = encoded(response)
        if len(output) > LIMIT:
            raise ValueError("output_limit")
        sys.stdout.buffer.write(output + b"\n")
    except (OSError, ValueError, KeyError, TypeError, UnicodeError, AttributeError,
            IndexError, RecursionError, OverflowError):
        sys.stderr.write("example_adapter_failed\n")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
