# Changelog

All notable changes to rindcut are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow [Semantic Versioning](https://semver.org/).

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

[0.1.0]: https://github.com/jeon-jihyeon/rindcut/releases/tag/v0.1.0
