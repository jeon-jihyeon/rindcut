# Changelog

All notable changes to rindcut are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow [Semantic Versioning](https://semver.org/).

## [0.2.0] - 2026-11-02

### Added

- A metadata-only live nodloop reader using its public MCP records contract
- Offline A2A Task and run usage source and observer examples with fixed evidence scopes, budget revisions and public contract scenarios
- Collect records from owner-configured agent sources and observations from separate programs, keeping source claims and observation results distinct
- Inspect observation records through the CLI and MCP, with cursor queries and JSONL export
- Include schemas, a source and observer contract, and examples for building adapters
- Suggest a session marker from a named Desktop task's prompt file

### Changed

- Start the shared five-minute collector after an approved enabled source save, including source-only setups
- Report actual scheduler health separately from installed files and preserve saved sources when scheduling fails
- Expose OTLP partial acceptance warnings and rejected span counts without replaying the accepted batch
- Use a bounded OTLP HTTP delivery adapter with system TLS trust and registered settings instead of SDK environment overrides
- Require Linux and macOS checks and a matching plugin version before publishing a release
- Use the same pinned Go compiler for tests and release builds
- Verify the GoReleaser archive before extraction and run only its verified binary without changing PATH

### Fixed

- Keep observation collection running when scheduled judging fails
- Protect another home's scheduler from replacement and reject nonregular lock files
- Report closed or malformed MCP test streams without waiting for the response timeout
- Honor an explicit Claude configuration directory in transcript backfill and installed tick schedules
- Match CLI help to flag ordering and the evidence scheduled checks actually establish
- Replay observation series in one batch while preserving immutable state and correction rules
- Reject links, directories and duplicate binary entries in downloaded release archives before caching them
- Preserve the plugin version when building the binary through the Go fallback
- Remove local source paths from release binaries and Go fallback builds

## [0.1.1] - 2026-10-25

### Added

- Optional content evaluation after objective checks pass, using registered criteria and a versioned adapter response
- Separate content judgments in reports, CLI output and MCP results, without changing objective verdicts or built-in exports
- Explicit explanation retention with bounded results; raw critic output stays out of the ledger

### Changed

- Describe a missed slot as unmatched session evidence rather than proof that a job never ran
- Explain the evidence access and transmission boundaries of external critic commands before registration

### Fixed

- Serialize job definition changes so concurrent saves and removals keep distinct revisions

## [0.1.0] - 2026-10-22

### Added

- Judge scheduled AI jobs from the real state of their output: file, http and command checks, with pass, fail, missed and inconclusive verdicts
- Append-only ledger of job revisions, sessions, verdicts with their evidence, and corrections
- `rindcut tick`, scheduled every 5 minutes with launchd or a systemd user timer on the first saved job
- Claude Code plugin: MCP tools to register jobs by conversation, a SessionEnd hook that records sessions in job folders, and a registration skill
- Exporters for OTLP backends like Langfuse and Phoenix, Langfuse scores, and nodloop outcomes
- MCP output schemas derived from existing result types with duration and raw JSON compatibility
- Final encoded verdict and probe size limits that retain outcomes and exact HTTP match evidence
- HTTP first-match offsets and read-limit metadata
- Command check scripts pinned by hash at preview and save, so an edited script makes its check inconclusive until the job is saved again
- The daily report counts closed slots with no verdict and shows a session that started after a missed slot's window
- `job_preview` warns about checks that give the same answer every slot
- The SessionEnd hook keeps a nodloop `run_id`, and each verdict carries the run id of its last matched session
- nodloop outcomes go to one bound run per verdict, saved in `cursors/nodloop-bindings.json` so corrections, retries and cursor resets reuse it
- Exporters keep entries that can't be delivered yet as pending and retry them before newer entries

### Changed

- Busy HTTP answers (429, 502, 503, 504) and command exit 75 are errors, so the verdict is inconclusive instead of fail
- Command output and trimmed ledger evidence keep both the start and the end, where failures are usually printed
- A fresh file changed at or after the next slot can't prove the earlier slot
- `job_list` and `verdicts` return their rows under `items`, so every MCP tool result is an object

### Fixed

- Command checks that could panic when the process never started
- A missed slot corrected by the owner now withdraws the nodloop outcome sent for its session

[0.2.0]: https://github.com/jeon-jihyeon/rindcut/releases/tag/v0.2.0
[0.1.1]: https://github.com/jeon-jihyeon/rindcut/releases/tag/v0.1.1
[0.1.0]: https://github.com/jeon-jihyeon/rindcut/releases/tag/v0.1.0
