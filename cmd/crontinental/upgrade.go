package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Upgrading is the installer's job, not this binary's: `upgrade` runs the
// install.sh that came in the same release, fed on stdin exactly as the
// one-liner feeds it, so it downloads, checks and swaps in the latest release
// the one way there is. All this side adds is knowing whether there is one.
//
// A release publishes VERSION beside its archives, so the latest is one small
// fetch from <releases>/latest/download/VERSION, which works the same against
// GitHub and against a file:// copy (CRONTINENTAL_RELEASES, as for install.sh).
// curl does the fetching, as it does for the installer: the binary carries no
// HTTP stack for one request.

const defaultReleases = "https://github.com/Techneosis/the-crontinental/releases"

const oneLiner = "curl -fsSL https://raw.githubusercontent.com/Techneosis/the-crontinental/main/install.sh | bash"

// exitNewer is `upgrade --check`'s status when a newer release is out, so a
// scheduled check reads as a failure and rings the notify hooks; 1 stays an
// error (no answer) and 0 means nothing to do.
const exitNewer = 3

func releasesURL() string {
	if r := os.Getenv("CRONTINENTAL_RELEASES"); r != "" {
		return strings.TrimRight(r, "/")
	}
	return defaultReleases
}

func latestRelease() string {
	if _, err := exec.LookPath("curl"); err != nil {
		die("upgrade needs curl, to ask the releases page")
	}
	url := releasesURL() + "/latest/download/VERSION"
	out, err := exec.Command("curl", "-fsSL", "--max-time", "30", url).Output()
	if err != nil {
		die("could not read the latest release from %s", url)
	}
	v := strings.TrimSpace(string(out))
	if _, _, ok := parseSemver(v); !ok {
		die("%s says the latest release is %q, which is not a version", url, v)
	}
	return v
}

// parseSemver reads vMAJOR.MINOR.PATCH with an optional -prerelease.
func parseSemver(s string) (nums [3]int, pre string, ok bool) {
	core, pre, _ := strings.Cut(strings.TrimPrefix(s, "v"), "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 || !strings.HasPrefix(s, "v") {
		return nums, "", false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nums, "", false
		}
		nums[i] = n
	}
	return nums, pre, true
}

// compareVersions is <0, 0 or >0 as a is older than, the same as, or newer
// than b. A prerelease comes before its release; two prereleases of the same
// version compare as text (rc1 < rc2, not semver's dotted rules).
func compareVersions(a, b string) int {
	an, ap, _ := parseSemver(a)
	bn, bp, _ := parseSemver(b)
	for i := range an {
		if an[i] != bn[i] {
			return an[i] - bn[i]
		}
	}
	switch {
	case ap == bp:
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	}
	return strings.Compare(ap, bp)
}

// installer finds the install.sh this binary was installed with: a release
// unpacks to <dir>/build/crontinental beside <dir>/install.sh, and the
// installer links ~/.local/bin to it.
func installer() string {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		die("cannot find this binary: %v", err)
	}
	dir := filepath.Dir(exe)
	root := filepath.Dir(dir)
	if _, err := os.Stat(filepath.Join(root, "cmd", "crontinental", "main.go")); err == nil {
		die("this crontinental is built from the checkout at %s; upgrade it there (git pull, then ./install.sh)", root)
	}
	script := filepath.Join(root, "install.sh")
	if _, err := os.Stat(script); err != nil || filepath.Base(dir) != "build" {
		die("%s was not installed by install.sh, so there is nothing to upgrade it with; install a release with:\n  %s", exe, oneLiner)
	}
	return script
}

func cmdUpgrade(o *opts) {
	if o.check {
		latest := latestRelease()
		switch {
		case version == "":
			fmt.Printf("this is a development build (%s); the latest release is %s\n", versionString(), latest)
		case compareVersions(latest, version) > 0:
			fmt.Printf("%s is out (this is %s); crontinental upgrade installs it\n", latest, version)
			os.Exit(exitNewer)
		case compareVersions(latest, version) == 0:
			fmt.Printf("%s is the latest release\n", version)
		default:
			fmt.Printf("this is %s, ahead of the latest release (%s)\n", version, latest)
		}
		return
	}

	script := installer()
	latest := latestRelease()
	if version != "" && compareVersions(latest, version) <= 0 {
		fmt.Printf("already up to date: this is %s, the latest release is %s\n", version, latest)
		return
	}
	from := version
	if from == "" {
		from = "a development build"
	}
	fmt.Printf("upgrading from %s to %s\n", from, latest)

	// Piped in, the installer has no file of its own, so it downloads rather
	// than reinstalling the release it sits in.
	f, err := os.Open(script)
	if err != nil {
		die("%v", err)
	}
	defer f.Close()
	cmd := exec.Command("bash", "-s", "--")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = f, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() > 0 {
			os.Exit(ee.ExitCode())
		}
		die("could not run the installer: %v", err)
	}
}
