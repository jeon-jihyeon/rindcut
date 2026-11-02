import contextlib
import copy
import io
import json
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

import eval_triggers


def successful_result() -> dict:
    names = ["register-agent-source", "watch-local-output", "explain-cron", "explain-http-spans"]
    return {"claudeVersion": "2.1.290", "partial": False, "cases": [
        {"name": name, "arms": {"with": [{"score": 1, "error": None,
          "graders": [{"passed": True, "scored": True, "explanation": "private-login-token"}]}]}}
        for name in names
    ]}


class TriggerRunnerTest(unittest.TestCase):
    def test_isolates_plugin_code_and_removes_run_files(self) -> None:
        seen = []

        def execute(command: list[str], **kwargs: object) -> subprocess.CompletedProcess[str]:
            target = Path(command[3])
            seen.append(target)
            self.assertEqual({".claude-plugin", "skills", "evals"}, {path.name for path in target.iterdir()})
            self.assertTrue((target / "skills" / "rindcut" / "SKILL.md").is_file())
            self.assertFalse((target / "hooks").exists())
            self.assertFalse((target / ".mcp.json").exists())
            self.assertIn("--no-publish", command)
            self.assertIn("--no-scaffold", command)
            self.assertEqual("record", command[command.index("--mocks") + 1])
            self.assertEqual("none", command[command.index("--ablation") + 1])
            self.assertEqual(target.parent, kwargs["cwd"])
            result = Path(command[command.index("--json") + 1])
            result.write_text(json.dumps(successful_result()))
            return subprocess.CompletedProcess(command, 0, "", "")

        with patch("eval_triggers.subprocess.run", side_effect=execute), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(0, eval_triggers.run(1, 2))
        self.assertEqual(1, len(seen))
        self.assertFalse(seen[0].exists())

    def test_preserves_native_failure(self) -> None:
        cases = [
            ("no result", False, 1, None, True),
            ("missing result despite native success", False, 0, None, True),
            ("partial authentication failure", True, 1, None, False),
            ("partial despite native success", True, 0, None, False),
            ("run error despite native success", False, 0, "private-login-token", False),
        ]
        for name, partial, code, failure, missing in cases:
            with self.subTest(name=name):
                def execute(command: list[str], **_kwargs: object) -> subprocess.CompletedProcess[str]:
                    if partial or failure:
                        result = Path(command[command.index("--json") + 1])
                        result.write_text(json.dumps({
                            "partial": partial, "partialReason": "auth_failed" if partial else None,
                            "cases": [{"arms": {"with": [{"error": failure}]}}],
                        }))
                    return subprocess.CompletedProcess(command, code, "private-authorization-value", "private-login-token\n")

                out, errors = io.StringIO(), io.StringIO()
                with (
                    patch("eval_triggers.subprocess.run", side_effect=execute),
                    contextlib.redirect_stdout(out), contextlib.redirect_stderr(errors),
                ):
                    self.assertEqual(1, eval_triggers.run(1, 1))
                self.assertEqual(missing, "Trigger evaluation produced no result" in errors.getvalue())
                self.assertNotIn("private-login-token", out.getvalue() + errors.getvalue())
                self.assertNotIn("private-authorization-value", out.getvalue() + errors.getvalue())

    def test_rejects_missing_cases_runs_and_grades(self) -> None:
        original = successful_result()
        empty = dict(original, cases=[])
        missing = dict(original, cases=original["cases"][:-1])
        duplicate = dict(original, cases=[original["cases"][0]] * 4)
        no_runs, no_grade, failed_grade, low_score, run_error = [copy.deepcopy(original) for _ in range(5)]
        no_runs["cases"][0]["arms"]["with"] = []
        no_grade["cases"][0]["arms"]["with"][0]["graders"] = []
        failed_grade["cases"][0]["arms"]["with"][0]["graders"][0]["passed"] = False
        low_score["cases"][0]["arms"]["with"][0]["score"] = 0
        run_error["cases"][0]["arms"]["with"][0]["error"] = "private-login-token"
        cases = [("complete", original, 0), ("empty", empty, 1), ("missing case", missing, 1),
                 ("duplicate case", duplicate, 1), ("missing run", no_runs, 1),
                 ("missing grade", no_grade, 1), ("failed grade", failed_grade, 1),
                 ("low score", low_score, 1), ("run error", run_error, 1),
                 ("partial", dict(original, partial=True), 1)]
        for name, result, expected in cases:
            with self.subTest(name=name):
                def execute(command: list[str], **_kwargs: object) -> subprocess.CompletedProcess[str]:
                    path = Path(command[command.index("--json") + 1])
                    path.write_text(json.dumps(result))
                    return subprocess.CompletedProcess(command, 0, "private-authorization-value", "private-login-token")

                out, errors = io.StringIO(), io.StringIO()
                with (
                    patch("eval_triggers.subprocess.run", side_effect=execute),
                    contextlib.redirect_stdout(out), contextlib.redirect_stderr(errors),
                ):
                    self.assertEqual(expected, eval_triggers.run(1, 1))
                self.assertNotIn("private-login-token", out.getvalue() + errors.getvalue())
                self.assertNotIn("private-authorization-value", out.getvalue() + errors.getvalue())


if __name__ == "__main__":
    unittest.main()
