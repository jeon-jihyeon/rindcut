# Changelog

All notable changes to rindcut are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- Judge scheduled AI jobs from the real state of their output: file, http and command checks, with pass, fail, missed and inconclusive verdicts
- Append-only ledger of job revisions, sessions, verdicts with their evidence, and corrections
- `rindcut tick`, scheduled every 5 minutes with launchd or a systemd user timer on the first saved job
- Claude Code plugin: MCP tools to register jobs by conversation, a SessionEnd hook that records sessions in job folders, and a registration skill
- Exporters for OTLP backends like Langfuse and Phoenix, Langfuse scores, and nodloop outcomes
