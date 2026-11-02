"""Run real skill selection checks without loading the plugin's hooks or servers."""

import argparse
import json
import math
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile


def score(value: object) -> float | None:
    if type(value) in (int, float) and 0 <= value <= 1 and math.isfinite(value):
        return float(value)
    return None


def summarize(result: dict, expected: set[str], runs: int, native_exit_code: int) -> dict:
    version = result.get("claudeVersion")
    if not isinstance(version, str) or not re.fullmatch(r"\d+\.\d+\.\d+", version):
        version = None
    summary = {"native_exit_code": native_exit_code, "exit_code": 1,
               "claude_version": version, "partial": result.get("partial") is not False,
               "cases": []}
    valid = bool(expected) and not native_exit_code and not summary["partial"]
    names = []
    for case in result.get("cases", []):
        name = case.get("name")
        if name not in expected:
            valid = False
            continue
        names.append(name)
        attempts = []
        for attempt in case.get("arms", {}).get("with", []):
            graders = [{"passed": grade.get("passed") is True, "scored": grade.get("scored") is True}
                       for grade in attempt.get("graders", [])]
            value = score(attempt.get("score"))
            error = bool(attempt.get("error"))
            valid = bool(valid and value == 1 and not error and graders
                         and all(grade["passed"] and grade["scored"] for grade in graders))
            attempts.append({"score": value, "error": "evaluation_run_failed" if error else None,
                             "graders": graders})
        valid = valid and len(attempts) == runs
        summary["cases"].append({"name": name, "runs": attempts})
    summary["exit_code"] = int(not (valid and len(names) == len(expected) and set(names) == expected))
    return summary


def run(runs: int, concurrency: int) -> int:
    plugin = Path(__file__).resolve().parent
    with tempfile.TemporaryDirectory(prefix="rindcut-trigger-eval-") as directory:
        workspace = Path(directory)
        target = workspace / "plugin"
        manifest = target / ".claude-plugin"
        manifest.mkdir(parents=True)
        shutil.copy2(plugin / ".claude-plugin" / "plugin.json", manifest / "plugin.json")
        shutil.copytree(plugin / "skills" / "rindcut", target / "skills" / "rindcut")
        shutil.copytree(
            plugin / "evals", target / "evals", ignore=shutil.ignore_patterns("results", "__pycache__")
        )
        result_path = workspace / "result.json"
        command = [
            "claude", "plugin", "eval", str(target),
            "--tag", "trigger", "--runs", str(runs), "--concurrency", str(concurrency),
            "--ablation", "none", "--mocks", "record", "--no-scaffold", "--no-publish",
            "--trust-plugin", "--output-dir", str(workspace / "results"),
            "--report", str(workspace / "report.html"), "--json", str(result_path),
        ]
        completed = subprocess.run(command, cwd=workspace, capture_output=True, text=True, check=False, timeout=900)
        if not result_path.exists():
            print("Trigger evaluation produced no result", file=sys.stderr)
            return completed.returncode or 1
        result = json.loads(result_path.read_text())
        expected = {path.parent.name for path in (target / "evals").glob("*/case.yaml")}
        summary = summarize(result, expected, runs, completed.returncode)
        print(json.dumps(summary, indent=2))
        return summary["exit_code"]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runs", type=int, choices=range(1, 51), default=3, metavar="1..50")
    parser.add_argument("--concurrency", type=int, choices=range(1, 9), default=3, metavar="1..8")
    args = parser.parse_args()
    try:
        return run(args.runs, args.concurrency)
    except (OSError, ValueError, TypeError, AttributeError, KeyError, subprocess.TimeoutExpired):
        print("Trigger evaluation failed", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
