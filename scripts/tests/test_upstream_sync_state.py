"""Exercise the sync classifier against real, disposable Git histories."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "upstream-sync-state.sh"


class SyncStateTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name)
        self.env = {
            **os.environ,
            "GIT_AUTHOR_NAME": "Sync test",
            "GIT_AUTHOR_EMAIL": "sync-test@example.invalid",
            "GIT_COMMITTER_NAME": "Sync test",
            "GIT_COMMITTER_EMAIL": "sync-test@example.invalid",
        }
        self.git("init", "--quiet")
        self.root = self.commit("root")

    def git(self, *args):
        return subprocess.check_output(
            ["git", *args], cwd=self.repo, env=self.env, text=True
        ).strip()

    def commit(self, message):
        self.git("commit", "--quiet", "--allow-empty", "-m", message)
        return self.git("rev-parse", "HEAD")

    def classify(self, downstream, upstream, expected):
        head_before = self.git("rev-parse", "HEAD")
        refs_before = self.git("show-ref")
        result = subprocess.run(
            ["bash", str(SCRIPT), downstream, upstream],
            cwd=self.repo, env=self.env, text=True, capture_output=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), expected)
        self.assertEqual(self.git("rev-parse", "HEAD"), head_before)
        self.assertEqual(self.git("show-ref"), refs_before)
        self.assertEqual(self.git("status", "--porcelain"), "")

    def test_equal_is_noop(self):
        self.classify(self.root, self.root, "equal")

    def test_fork_only_governance_commit_is_noop(self):
        downstream = self.commit("downstream governance")
        self.classify(downstream, self.root, "upstream-contained")

    def test_upstream_only_commit_can_fast_forward(self):
        upstream = self.commit("upstream change")
        self.classify(self.root, upstream, "fast-forward")

    def test_both_sides_changed_requires_review(self):
        downstream = self.commit("downstream change")
        self.git("checkout", "--quiet", "--detach", self.root)
        upstream = self.commit("upstream change")
        self.classify(downstream, upstream, "diverged")

    def test_unrelated_histories_require_review(self):
        self.git("checkout", "--quiet", "--orphan", "unrelated")
        upstream = self.commit("unrelated root")
        self.classify(self.root, upstream, "diverged")

    def test_invalid_revision_fails_instead_of_reporting_divergence(self):
        result = subprocess.run(
            ["bash", str(SCRIPT), self.root, "missing-branch"],
            cwd=self.repo, env=self.env, text=True, capture_output=True,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout, "")


if __name__ == "__main__":
    unittest.main()
