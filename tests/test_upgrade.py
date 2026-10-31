"""`upgrade`, against a throwaway releases directory (CRONTINENTAL_RELEASES).

The installer it runs is a stand-in that only reports how it was called, so
nothing here touches systemd or ~/.local; the real installer's download path
is exercised end to end by the release workflow.

    python3 -m unittest discover tests
"""

import atexit
import os
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

from _binary import BIN, ROOT

RELEASE = re.compile(r"^crontinental (v\d+\.\d+\.\d+(?:-\S+)?) ")
NEWER = 3

# Records how upgrade ran it: piped on stdin (no file of its own, so the real
# installer would download) and with the caller's releases URL.
STUB = """\
echo "installer: source=${BASH_SOURCE[0]:-none} releases=$CRONTINENTAL_RELEASES args=$*"
exit ${STUB_EXIT:-0}
"""


def _version(path):
    return subprocess.run([path, "version"], capture_output=True, text=True, check=True).stdout


def released():
    """A binary stamped with a release version, and that version: the one under
    test if it is a release build, else this checkout built as one."""
    m = RELEASE.match(_version(BIN))
    if m:
        return BIN, m.group(1)
    out = tempfile.mkdtemp(prefix="crontinental-test-")
    atexit.register(shutil.rmtree, out, ignore_errors=True)
    path = os.path.join(out, "crontinental")
    subprocess.run(["go", "build", "-ldflags", "-X main.version=v1.2.3", "-o", path,
                    "./cmd/crontinental"], cwd=ROOT, check=True,
                   env={**os.environ, "CGO_ENABLED": "0"})
    return path, "v1.2.3"


REL_BIN, CURRENT = released()


def bump(v, newer):
    major = int(v[1:].split(".")[0])
    return f"v{major + 1}.0.0" if newer else "v0.0.0-0"


class Upgrade(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.t = Path(self.tmp.name)
        self.latest = self.t / "releases" / "latest" / "download"
        self.latest.mkdir(parents=True)
        self.env = {**os.environ, "CRONTINENTAL_RELEASES": f"file://{self.t}/releases",
                    "CRONTINENTAL_DB": str(self.t / "ledger.db")}

    def tearDown(self):
        self.tmp.cleanup()

    def publish(self, version):
        (self.latest / "VERSION").write_text(version + "\n")

    def install(self, kind="release"):
        """A copy of the release binary laid out as install.sh leaves one."""
        d = self.t / kind
        (d / "build").mkdir(parents=True)
        shutil.copy(REL_BIN, d / "build" / "crontinental")
        if kind == "release":
            (d / "install.sh").write_text(STUB)
        else:
            (d / "cmd" / "crontinental").mkdir(parents=True)
            (d / "cmd" / "crontinental" / "main.go").write_text("package main\n")
        # reached through a link, as ~/.local/bin/crontinental is
        link = self.t / "bin" / "crontinental"
        link.parent.mkdir(exist_ok=True)
        link.symlink_to(d / "build" / "crontinental")
        return str(link)

    def run_(self, binary, *args):
        return subprocess.run([binary, "upgrade", *args], env=self.env,
                              capture_output=True, text=True, stdin=subprocess.DEVNULL)

    # --check

    def test_check_up_to_date(self):
        self.publish(CURRENT)
        p = self.run_(REL_BIN, "--check")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn(f"{CURRENT} is the latest release", p.stdout)

    def test_check_finds_a_newer_release_and_says_so_in_its_status(self):
        self.publish(bump(CURRENT, newer=True))
        p = self.run_(REL_BIN, "--check")
        self.assertEqual(p.returncode, NEWER, p.stderr)
        self.assertIn(f"{bump(CURRENT, True)} is out (this is {CURRENT})", p.stdout)

    def test_check_ahead_of_the_latest(self):
        self.publish(bump(CURRENT, newer=False))
        p = self.run_(REL_BIN, "--check")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn("ahead of the latest release", p.stdout)

    def test_a_prerelease_is_older_than_its_release(self):
        if "-" in CURRENT:
            self.skipTest("the binary under test is itself a prerelease")
        self.publish(CURRENT + "-rc1")
        p = self.run_(REL_BIN, "--check")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn("ahead of the latest release", p.stdout)

    def test_check_on_a_development_build_names_the_latest_without_judging(self):
        if RELEASE.match(_version(BIN)):
            self.skipTest("the binary under test is a release build")
        self.publish("v9.9.9")
        p = self.run_(BIN, "--check")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn("development build", p.stdout)
        self.assertIn("v9.9.9", p.stdout)

    def test_no_answer_is_an_error_not_up_to_date(self):
        p = self.run_(REL_BIN, "--check")  # nothing published
        self.assertEqual(p.returncode, 1)
        self.assertIn("could not read the latest release", p.stderr)

    def test_a_garbled_answer_is_refused(self):
        self.publish("<html>rate limited</html>")
        p = self.run_(REL_BIN, "--check")
        self.assertEqual(p.returncode, 1)
        self.assertIn("not a version", p.stderr)

    # upgrade

    def test_upgrade_runs_the_installer_piped_so_it_downloads(self):
        self.publish(bump(CURRENT, newer=True))
        p = self.run_(self.install())
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn(f"upgrading from {CURRENT} to {bump(CURRENT, True)}", p.stdout)
        self.assertIn(f"installer: source=none releases=file://{self.t}/releases args=", p.stdout)

    def test_the_installers_failure_is_the_upgrades(self):
        self.publish(bump(CURRENT, newer=True))
        self.env["STUB_EXIT"] = "7"
        p = self.run_(self.install())
        self.assertEqual(p.returncode, 7)

    def test_up_to_date_does_not_run_the_installer(self):
        self.publish(CURRENT)
        p = self.run_(self.install())
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn("already up to date", p.stdout)
        self.assertNotIn("installer:", p.stdout)

    def test_a_checkout_build_is_upgraded_in_its_checkout(self):
        self.publish(bump(CURRENT, newer=True))
        p = self.run_(self.install("checkout"))
        self.assertEqual(p.returncode, 1)
        self.assertIn("built from the checkout", p.stderr)
        self.assertNotIn("installer:", p.stdout)

    def test_a_loose_binary_points_at_the_installer(self):
        self.publish(bump(CURRENT, newer=True))
        loose = self.t / "loose"
        shutil.copy(REL_BIN, loose)
        p = self.run_(str(loose))
        self.assertEqual(p.returncode, 1)
        self.assertIn("not installed by install.sh", p.stderr)
        self.assertIn("install.sh | bash", p.stderr)


if __name__ == "__main__":
    unittest.main()
