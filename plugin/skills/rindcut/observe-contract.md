# Observation adapter contract

rindcut records metadata from an owner-configured source and observations from separate owner-configured programs. A source record is a producer claim. An observation is a result under a registered criterion; it is not proof of a business outcome or permission to act.

The published JSON Schemas describe the wire shapes: [definition](https://github.com/jeon-jihyeon/rindcut/blob/main/schemas/observe/definition.schema.json), [source request](https://github.com/jeon-jihyeon/rindcut/blob/main/schemas/observe/source-request.schema.json), [source response](https://github.com/jeon-jihyeon/rindcut/blob/main/schemas/observe/source-response.schema.json), [observer request](https://github.com/jeon-jihyeon/rindcut/blob/main/schemas/observe/observer-request.schema.json), and [observer response](https://github.com/jeon-jihyeon/rindcut/blob/main/schemas/observe/observer-response.schema.json). Use the schemas from the release that matches the installed binary. Schema validation alone cannot check byte limits, duplicate keys, lexical integer forms, timestamp validity, or values that must match the request. In [conformance fixtures](https://github.com/jeon-jihyeon/rindcut/blob/main/schemas/observe/conformance.json), `valid` is the parsed JSON Schema result and optional `wire_valid` is the raw runtime result. Preserve the original `document` bytes when checking lexical rules.

```text
source → reader → event → observation ledger → cursor query
external state → observer → observation → observation ledger
```

## Configure a source

Call `source_preview` with `definition` and `expected_revision`: use `0` for a new source and the current revision for a change. Show the owner the returned definition, resolved programs, arguments, working directories, fingerprints, retained metadata, effects and schedule target. Call `source_save` only after the owner agrees, with the same request, `preview_digest` and `confirmed: true`. A changed definition, executable, checkpoint or schedule target needs a fresh preview. Confirmation records the caller's choice; it does not authenticate a person.

For an enabled source, the preview includes `tick_schedule_ensure` and a `schedule` object naming the OS, user id, program, home, user home, optional Claude config directory and schedule name, a 300-second interval and the `ensure` effect. The digest binds this target and policy, not a momentary running state. After the source is durably saved and its control lock released, registration ensures the shared tick without waiting for its first collection. The OS may start that tick right away. Preview and rejected saves never start it. A disabled source starts no schedule, and removing a source leaves the shared schedule available to other sources and jobs.

The saved answer has separate `source` and `schedule` results. A scheduling failure leaves the source saved and returns a fixed schedule `state` and `reason`. Optional `changed: true` means this call activated and verified the schedule under the shared lock; a reused schedule omits it. Retry the existing `rindcut install` path to repair the schedule without creating another source revision. `status.schedule` checks OS registration and the execution target; an installed file or `last_tick` alone does not prove collection, observation or export success. A schedule for another home or an unrecognized execution target is never replaced automatically.

The smallest source definition can collect without an observer:

```json
{
  "id": "acme",
  "enabled": true,
  "source_instance": "acme-store",
  "stream": "actions",
  "capability": {"kind": "event_stream", "types": [], "limitations": []},
  "adapter_revision": "v1",
  "projection_profile": "metadata-v1",
  "reader": {"program": "/path/to/acme-reader", "args": [], "allowed_attributes": []},
  "observers": []
}
```

`id` is a non-reusable registration identity; rindcut assigns positive source and observer revisions. `source_instance` identifies the source's durable store, `stream` its stream, `adapter_revision` the adapter version and `projection_profile` the metadata projection. `capability.kind` is `event_stream` or `snapshot`; `types` and `limitations` are arrays. A snapshot reports samples, not the intermediate attempts between them. Optional `description` is at most 2048 UTF-8 bytes.

The reader requires `program`, an `args` array and an `allowed_attributes` array. Its optional `limit` defaults to 100 and cannot exceed 100. Each reader or observer command may set `timeout_seconds`, from 1 to 600, default 30, and up to 16 `fingerprint_files`. Each `args` array has at most 128 strings of at most 2048 UTF-8 bytes each. Add script or other input files to `fingerprint_files` when their contents must be pinned. The executable and declared files are checked again before execution; an unreadable or changed fingerprint stops execution with `program_changed`. Fingerprints do not cover undeclared libraries or external state.

Each observer requires `id`, a nonempty `types` array, `criterion_id`, `criterion_revision`, `parameters`, `program`, `args` and `deadline_seconds`. Observer ids are unique within the source. `interval_seconds` and `deadline_seconds` range from 1 to 31536000; interval defaults to 60. `mode` defaults to `until_terminal` and may be `watch`. `actor_kind` defaults to `code` and may be `model`; it is the owner's declaration, not certification of the program's implementation, independence or accuracy. A definition contains at most 100 observers. A definition and each adapter wire JSON object are at most 1 MiB.

Changing collection identity or generation stops collection and records a gap. Choose an explicit `backfill` or `reset` in a fresh preview; do not silently reset a cursor. A reset preview binds the requested generation into the plan without running the reader. After confirmation, save checks the preview digest and probes the reader for that generation before committing. Backfill starts at the first cursor in the same generation; reset starts a new collection boundary. A `new_store` recovery preserves the previous store separately and starts an empty store with a new identity; its earlier cursors are invalid and sources must be registered again.

## Wire values and execution

Both commands receive one JSON object on stdin and must emit one JSON object on stdout, with no trailing output. Unknown keys, duplicate keys, invalid UTF-8, unpaired surrogates, wrong types and unexpected nulls are rejected. Null is allowed for source request `generation` and `cursor`, response `next_cursor`, and response `generation` when unavailable. Omit other optional fields instead of sending null.

An identifier is nonempty UTF-8 of at most 256 bytes, without control characters. Timestamps are RFC 3339 with a timezone and at most nine fractional digits. A digest is `sha256:` followed by 64 lowercase hexadecimal digits. Attributes are an object of at most 32 keys, each matching `[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}`. Values are UTF-8 strings of at most 2048 bytes, booleans or integers from -9007199254740991 to 9007199254740991. Decimal points, exponents, negative zero, null, arrays and objects are not attribute values.

An event has at most 32 `links` and 16 `evidence` entries. Each link has a `relation` and a `target` containing `source_id`, `source_instance`, `generation`, `stream`, `type`, `id` and `projection_profile`. Each evidence item has `kind`, `ref` and an optional `digest`. Its `ref` has `namespace` and `id`, each matching `[A-Za-z0-9][A-Za-z0-9_.:-]{0,255}`; do not put URLs or file paths there. All arrays and objects required by the schema must be present even when empty.

Commands are invoked directly with the registered argv, not constructed from events. They run with the owner's OS permissions and are not sandboxed. Preview pins the working directory for later execution. Each response is limited to 1 MiB and each resulting ledger line to less than 64 KiB; rindcut does not truncate identities or references to make them fit. Timeout or cancellation terminates the process group. A sync cycle has a 60-second budget and at most 100 external calls; each call also respects the remaining budget. Raw stderr and invalid stdout are not copied into the ledger or errors. Failures use fixed reason codes; failed or interrupted work can be attempted in a later cycle. No producer action is retried.

Do not put credentials, prompts, raw payloads, authorization links or continuation URLs in definitions, returned metadata or evidence. Use the adapter's existing authentication path. `allowed_attributes` controls which keys are accepted, not whether their values are safe: the owner must review retained values. Source-supplied `person`, `verified`, authority, proof, links and evidence remain source claims. A declared observer actor does not grant human authority or establish correctness.

## Source reader

The request uses `protocol: "rindcut.source/v1"`, `operation: "read"`, `source_id`, positive `source_revision`, `source_instance`, `stream`, `generation`, `cursor` and `limit`. The first request has null generation and cursor. A response echoes the protocol, source identity and revision, instance and stream, and returns the configured `capability`, `status`, `records`, `generation`, `next_cursor`, `has_more` and `coverage`.

For example, a response to the first request for the definition above may be:

```json
{
  "protocol": "rindcut.source/v1", "source_id": "acme", "source_revision": 1,
  "source_instance": "acme-store", "stream": "actions", "generation": "g1",
  "capability": {"kind": "event_stream", "types": [], "limitations": []},
  "status": "ok", "records": [{
    "source_instance": "acme-store", "generation": "g1", "stream": "actions",
    "id": "event-1", "type": "acme.completed", "schema_version": "v1",
    "adapter_revision": "v1", "projection_profile": "metadata-v1",
    "occurred_at": "2026-01-01T00:00:00Z", "links": [], "attributes": {}, "evidence": []
  }],
  "next_cursor": "offset-1", "has_more": false,
  "coverage": {"kind": "event_stream", "limitations": []}
}
```

An event requires `source_instance`, `generation`, `stream`, `id`, `type`, `schema_version`, `adapter_revision`, `projection_profile`, `links`, `attributes` and `evidence`. Optional `producer_version`, `occurred_at`, `action_id` and `attempt_id` are omitted when unknown. The instance, stream, adapter revision and projection must match the definition; generation must match the page. Every attribute key must be allowed by the reader definition. A single invalid event rejects the whole page and leaves the checkpoint unchanged.

`ok` requires a generation. Rindcut pins the first successful generation and passes it back with the adapter-owned opaque cursor. A page contains no more than the requested limit, from 1 to 100. If `has_more` is true or records are present, `next_cursor` must be non-null and differ from the request cursor, including for an empty page with more data. At the current end return `has_more: false`; the next cycle reads again from the last cursor. Null at the end is allowed only for an empty initial source. Cursors are at most 8192 bytes.

`cursor_invalid` returns an empty records array, null `next_cursor`, false `has_more`, the known generation and coverage. `unavailable` has the same empty page shape, may use null generation and includes `unavailable` in coverage limitations. Neither advances the checkpoint. Coverage has `kind` (`event_stream`, `snapshot` or `unknown`), a `limitations` array and optional `window_start` and `window_end`; its range is a source claim, not proof that every action was captured.

Rindcut commits the complete page and its required pending work before advancing the checkpoint. A crash before checkpoint publication can replay that page, so return stable identities and projections for replay. Within the same registered source, instance, generation, stream, type and id, an identical projection is a retransmission; a different projection is retained as a conflict. A different projection profile is a separate comparison scope. Collection identity changes require explicit recovery instead of moving an old cursor to a different reader. An explicit backfill can create a new observer revision's series for replayed historical captures; ordinary replay reuses existing captures and series.

## Observer

The request uses `protocol: "rindcut.observer/v1"`, `source_id`, `source_revision`, `observer_id`, `observer_revision`, `criterion_id`, `criterion_revision`, `series_id`, `target_ref`, `parameters`, `event` and `now`. `target_ref` contains the `store_id` and `capture_id` of the exact capture. `event` is that fixed capture, including its source revision, capture id, captured time and normalized digest. The request's source revision is the observation definition revision; the event's source revision is the original capture revision and can differ after backfill. `previous_observation_id` is absent on the first call.

The response must echo protocol, source id and revision, observer id and revision, criterion id and revision, series id and target ref. It also requires `result`, `reason_code` and `evidence`. Results are `confirmed`, `refuted`, `pending` or `unknown`; `reason_code` matches `[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}` and contains no free-form explanation. Rindcut assigns `observation_id` and `observed_at`. A timeout, cancellation, nonzero exit or invalid response yields `unknown`, not `refuted`.

An optional `next_observe_at` must be later than request `now` and must still be later than the collector's `observed_at` when the response is recorded. If it expires during execution, rindcut records `unknown` with `reason_code: "invalid_response"`. For a valid value, the next attempt is the later of that time and `observed_at` plus the configured interval; omit it to use the interval. Pending and retryable unknown results are observed again in later cycles even if no new source event arrives. `until_terminal` ends on confirmed or refuted; `watch` continues at the interval. A passed deadline marks the series overdue and does not create a failure or stop its normal observation policy.

A later observation does not automatically correct an earlier one. To explicitly correct an observation in the same store, capture, series and criterion revision, return `supersedes` with its `store_id` and `observation_id`. Rindcut preserves the earlier observation. A changed criterion creates a separate series. Disabling or removing a source stops future work without turning prior results into refutations.

## Projection digest

Rindcut computes the digest of a deterministic JSON array in this order: protocol `rindcut.projection/v1`, `source_instance`, `generation`, `stream`, `id`, `type`, `schema_version`, `producer_version`, `adapter_revision`, `projection_profile`, `occurred_at`, `action_id`, `attempt_id`, `links`, `attributes`, `evidence`. Missing optional fields become null. Normalize timestamps to UTC with nine fractional digits. Transform each link into `[relation, [source_id, source_instance, generation, stream, type, id, projection_profile]]` and each evidence item into `[kind, namespace, id, digest]`, using null for a missing digest. Transform attributes into `[key, value]` pairs sorted by ASCII key; preserve the order of other arrays.

Encode UTF-8 JSON with no whitespace or trailing newline. Use decimal integers and lowercase booleans and null. Escape quotes and backslashes, not slash. Use `\b`, `\f`, `\n`, `\r`, `\t` for those controls and lowercase `\u00xx` for other C0 controls. Escape `<`, `>`, `&`, U+2028 and U+2029 as lowercase `\u` escapes; leave other valid Unicode in UTF-8 without normalization. SHA-256 these bytes. Capture id, captured time, source revision and public cursor are excluded. The [projection vectors](https://github.com/jeon-jihyeon/rindcut/blob/main/schemas/observe/projection-vectors.json) let implementations in another language check canonical bytes and digests. The digest compares projections; it does not authenticate the producer or prove a claim true.

## Offline A2A and usage profiles

The repository's `examples/observe` directory includes `a2a.py` and `usage.py`, ready source definitions, immutable input journals, separate evidence resolvers and literal expected manifests. Run them from the repository root with Python 3.10 or later. Review and replace the example paths and scopes before calling `source_preview` and `source_save`. Both scripts use the common protocols above; neither connects to a live agent, provider or arbitrary artifact URL.

`nodloop_live.py` reads one record kind from an owner-fixed nodloop MCP program with `content: false`. Its example definition pins the child binary and uses an empty observer list. It keeps published identity, time, version and known enum claims, and drops free text, labels, sessions, reviewers, paths and URLs. `guard` is not supported. The declared generation is not independent store attestation; pin the record directory for a long-lived registration. Removed native cursors require explicit reset. This reader checks collection fidelity, not an independent business outcome or human authorization.

The collector definition passes `--managed-process-group` so the native MCP child inherits the reader group. This mode requires the reader to lead that group; the reader reaps its direct child and the collector removes remaining group members on return or cancellation. Standalone use creates and cleans up a separate child group. The native MCP handshake and record request share one deadline across nonblocking writes and reads. The definition also fingerprints the shared stdio helper; changing it requires a fresh preview.

```text
immutable snapshot → external reader → event → existing observation store
fixed criterion and separate evidence → external observer → observation
record query → external consumer or execution controller
```

| Profile | Recorded claim | Independent comparison within the supplied fixture scope |
|---|---|---|
| A2A wire 1.0, specification 1.0.1 | Immutable Task sample with a distinct sample id, scoped Task reference, metadata and fixed artifact version | Registered destination scope and closed time window, required effects and artifacts, cancellation cutoff, explicit idempotency scope and artifact byte digest |
| Run usage | Immutable partial or final snapshot with budget revision, meter profile and integer counters | Fixed budget digest, completeness manifest and versioned native receipts bound to provider, API, model, run, attempt and accounting plane |

For A2A, `COMPLETED` is a source claim. Optional artifacts become required only through the criterion. Unknown chunk coverage, missing versions, expired read access and unknown cancellation order remain unknown. Missing effects in open coverage remain pending; absence can refute a requirement only within its fixed closed window. Distinct Tasks sharing a context are not automatically duplicates. These examples do not establish A2A protocol conformance or general answer correctness. The [pinned A2A specification](https://github.com/a2aproject/A2A/blob/v1.0.1/docs/specification.md) defines the protocol independently.

For usage, the capability includes `acme.run_usage.partial` and `acme.run_usage.final`, but the supplied observer selects final only. Partial snapshots remain recorded with no observation series. A later final has a new identity and explicit replacement link. Consumers select one cumulative snapshot, never sum partial and final values. Ambiguous replacement scope stays unresolved. The helper `select_snapshot` requires captured events with `source_id`; a raw source page consumer must add its envelope's `source_id` before selection.

The fixed budget uses inclusive upper bounds: equality passes, an excess refutes, and a required missing dimension stays unknown. OpenAI cached input and reasoning output are subsets of native totals; Anthropic input includes native input, cache creation and cache read. Cumulative streaming output is not summed across chunks. Actual retry consumption counts separately from receipt redelivery, and explicit inclusive parent or duplicate receipts are not counted twice. Direct client tool calls include failures; elapsed time uses one clock. These are pinned normalization profiles, not interchangeable prices or billing verification. See [OpenAI usage](https://developers.openai.com/api/reference/resources/completions#completion-usage), [Anthropic caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching#tracking-cache-performance) and the [pinned OTel GenAI convention](https://github.com/open-telemetry/semantic-conventions-genai/blob/dc3571562603552dc555c667dfa327a638d94ab2/docs/gen-ai/client-inference.md).

```sh
python3 -m unittest discover -s examples/observe -p 'test_*.py'
python3 examples/observe/profile_scenario.py --binary /path/to/rindcut
```

The tests check literal expected values, deliberately broken normalization and judgments, and the shared CLI and MCP paths. The scenario prints its binary digest and assertion results. Success establishes fixture behavior only. Provider accounts, a protocol server, production destinations, long running schedules and backend storage remain separate checks. No result stops an agent, retries its work, authenticates human approval or changes a scheduled verdict.
