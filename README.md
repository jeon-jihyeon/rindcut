<div align="center">
  <h1>rindcut</h1>
  <h3>Checks that your scheduled AI jobs really ran and really produced what they promised.</h3>
</div>

<div align="center">
  <a href="https://github.com/jeon-jihyeon/rindcut/releases"><img src="https://img.shields.io/github/v/release/jeon-jihyeon/rindcut?display_name=tag" alt="release"></a>
  <a href="https://github.com/jeon-jihyeon/rindcut/actions/workflows/test.yml"><img src="https://img.shields.io/github/actions/workflow/status/jeon-jihyeon/rindcut/test.yml?branch=main&label=test" alt="test"></a>
  <a href="https://goreportcard.com/report/github.com/jeon-jihyeon/rindcut"><img src="https://goreportcard.com/badge/github.com/jeon-jihyeon/rindcut" alt="go report"></a>
  <img src="https://img.shields.io/badge/license-MIT-green" alt="license">
</div>

<br>

A scheduled Claude task shows green. The report it was supposed to write never landed.

A schedule only knows that a run ended. It doesn't know whether the work exists. rindcut cuts the rind and looks inside: for every slot a job was due, it checks the real output, the file, the page or whatever your own script looks at, and records one verdict on your machine.

## Quickstart

```
/plugin marketplace add jeon-jihyeon/rindcut
/plugin install rindcut@rindcut
```

Then tell Claude what to watch, in your own words:

```
> make sure the 9am price collection really happened. It writes out/<date>.csv in ~/price
```

Claude asks what it needs one question at a time, shows you what each check sees right now, and saves the job only after you agree. The first saved job schedules the checker every 5 minutes with launchd on macOS or a systemd user timer on Linux. You never edit a file or type a command.

Ask any time:

- "Did the price job work today?" and why not, from the evidence the check saw
- "What happened yesterday?" for a summary with problems, silent jobs and export lag
- "That verdict is wrong" to record a correction. The original verdict stays on record

## Verdicts

| Verdict | Meaning |
|---|---|
| `pass` | every check passed |
| `fail` | a check looked and the output isn't right |
| `missed` | the job runs on this machine and no matching session started in its window, so it likely never ran |
| `inconclusive` | a check couldn't look, like no network, so nothing is known. Never counted as a failure |

A slot is judged once its window closes, against the job as it was at that time, so a later edit never rewrites an earlier verdict.

## How it works

```
schedule → Claude session → output on disk, a page or anything a script reads
                ↓                                ↓
         SessionEnd hook                 checks every 5 minutes
                ↓                                ↓
                → ledger.jsonl, one verdict per slot ←
                                ↓
              report, and exporters to your tracing tool
```

- Job: a name, a schedule with its timezone and grace, and the checks that prove it worked. `file` looks at a path with `{{date}}` for the slot's date, `http` looks for text on a page, and `command` runs a script that exits 0 on success
- Session match: for a job that runs on this machine, a session counts only when it started in the job's folder, inside the slot's window, with the scheduled prompt's first line. A session you open by hand there doesn't count
- Cloud jobs: a Cowork schedule or a Routine is judged from its output alone, so missed and fail read the same
- Ledger: `~/.rindcut/ledger.jsonl`, append only. Corrections are new entries. Only sessions in a job's folder are recorded, and only verdicts and corrections leave the machine

## Exporters

rindcut draws no screens and sends no messages. Verdicts go where you already look.

| Exporter | Sends |
|---|---|
| `otlp` | one trace per verdict with a span per check to Langfuse, Phoenix, Opik, LangSmith or any OpenTelemetry backend. `session.id` joins it to the Claude Code run |
| `langfuse-score` | verdicts and corrections as Langfuse scores |
| `nodloop` | each verdict as an outcome on the [nodloop](https://github.com/jeon-jihyeon/nodloop) run it judged, on by default when nodloop is installed |

Ask Claude to connect one. It sends a test right away and never repeats your keys back.

## Commands

The plugin is all an owner needs. The CLI is there for scripts and for looking around.

| Command | What it does |
|---|---|
| `rindcut tick` | judge closed slots and run the exporters, what the schedule calls |
| `rindcut check JOB` | dry run of a job's latest slot |
| `rindcut jobs`, `ledger`, `report` | list jobs, ledger entries and the summary. `--json` for machines |
| `rindcut export` | send new entries to the exporters |
| `rindcut install`, `uninstall` | schedule or unschedule the tick |
| `rindcut status` | version, home and tick state |

`RINDCUT_HOME` moves `~/.rindcut`. Exit codes are 0 done, 1 failed and 2 usage error.

## Install without the plugin

```
go install github.com/jeon-jihyeon/rindcut/cmd/rindcut@latest
```

The plugin launcher fetches the release that matches its version and checks it against `checksums.txt`. Releases carry build provenance, so `gh attestation verify <archive> --repo jeon-jihyeon/rindcut` shows an archive was built here from its tag.

## License

[MIT](LICENSE)
