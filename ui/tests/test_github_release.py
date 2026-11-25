"""Exercise stable release publication with a fake gh executable."""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts/publish-github-release.sh"
FAKE_GH = """#!/usr/bin/env python3
import json
import os
import sys

args = sys.argv[1:]
with open(os.environ['MOCK_CALLS'], 'a') as log:
    log.write(json.dumps(args) + '\\n')
if args[:2] == ['release', 'view']:
    mode = os.environ.get('MOCK_RELEASE', 'missing')
    if mode == 'missing':
        raise SystemExit(1)
    print('true' if mode == 'draft' else 'false')
elif args[:2] not in [['release', 'create'], ['release', 'edit']]:
    raise SystemExit(3)
"""


class GitHubReleaseTests(unittest.TestCase):
    def execute(self, ref="refs/tags/v0.2.0", tag="v0.2.0", mode="missing", notes=True):
        with tempfile.TemporaryDirectory(prefix="prismvox-release-test-") as directory:
            root = Path(directory)
            binary = root / "bin/gh"
            binary.parent.mkdir()
            binary.write_text(FAKE_GH)
            binary.chmod(0o755)
            if notes:
                release = root / "docs/releases/v0.2.0.md"
                release.parent.mkdir(parents=True)
                release.write_text("# PrismVox 0.2.0\n\nRelease notes.\n")
            calls_file = root / "calls.jsonl"
            env = dict(
                os.environ,
                PATH=str(binary.parent) + ":" + os.environ["PATH"],
                GITHUB_REF=ref,
                RELEASE_TAG=tag,
                GITHUB_REPOSITORY="fixture/PrismVox",
                RUNNER_TEMP=str(root),
                MOCK_CALLS=str(calls_file),
                MOCK_RELEASE=mode,
            )
            result = subprocess.run(
                ["bash", str(SCRIPT)], cwd=root, env=env,
                capture_output=True, text=True,
            )
            calls = (
                [json.loads(line) for line in calls_file.read_text().splitlines()]
                if calls_file.exists() else []
            )
            body = root / "prismvox-release.md"
            return result, calls, body.read_text() if body.exists() else None

    def test_new_stable_release_requires_existing_tag_and_uses_notes(self):
        result, calls, body = self.execute()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([call[:2] for call in calls], [
            ["release", "view"], ["release", "create"],
        ])
        self.assertIn("--verify-tag", calls[-1])
        self.assertIn("--latest", calls[-1])
        self.assertEqual(calls[-1][calls[-1].index("--title") + 1], "PrismVox 0.2.0")
        self.assertEqual(body, "\nRelease notes.\n")

    def test_existing_public_release_is_not_rewritten(self):
        result, calls, _ = self.execute(mode="published")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([call[:2] for call in calls], [["release", "view"]])

    def test_existing_draft_is_published_as_stable_latest(self):
        result, calls, _ = self.execute(mode="draft")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls[-1][:2], ["release", "edit"])
        for argument in ["--draft=false", "--prerelease=false", "--latest"]:
            self.assertIn(argument, calls[-1])

    def test_nonstable_refs_never_contact_github(self):
        for ref in [
            "refs/heads/main", "refs/tags/v0.2.0-rc.1", "refs/tags/v0.2.0+build",
            "refs/tags/vnext", "refs/tags/v00.2.0", "refs/tags/v0.2",
        ]:
            with self.subTest(ref=ref):
                result, calls, _ = self.execute(ref=ref)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(calls, [])

    def test_missing_notes_or_mismatched_tag_never_contact_github(self):
        for changes in [{"notes": False}, {"tag": "v0.2.1"}]:
            with self.subTest(changes=changes):
                result, calls, _ = self.execute(**changes)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(calls, [])


if __name__ == "__main__":
    unittest.main()
