package domain

import "golang.org/x/mod/semver"

// ReleaseCompatible reports whether a worker release implements the core
// release's contract: same major and minor (docs/adr/0005). known is false
// when either side is not a release tag (dev build, bare commit), which
// exempts it from the check.
func ReleaseCompatible(core, worker string) (compatible, known bool) {
	c, w := semver.MajorMinor(core), semver.MajorMinor(worker)
	if c == "" || w == "" {
		return true, false
	}
	return c == w, true
}
