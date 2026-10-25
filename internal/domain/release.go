package domain

import (
	"strconv"
	"strings"

	"golang.org/x/mod/semver"
)

// ReleaseCompatible reports whether a worker release works with the core
// release (docs/adr/0005). Before 1.0 both must share major.minor. From 1.0
// on, minor and patch releases are backwards compatible: same major and the
// worker's minor not above core's. known is false when either side is not a
// release tag (dev build, bare commit), which exempts it from the check.
func ReleaseCompatible(core, worker string) (compatible, known bool) {
	cMaj, cMin, cok := releaseMajorMinor(core)
	wMaj, wMin, wok := releaseMajorMinor(worker)
	if !cok || !wok {
		return true, false
	}
	switch {
	case cMaj != wMaj:
		return false, true
	case cMaj == 0:
		return cMin == wMin, true
	}
	return wMin <= cMin, true
}

func releaseMajorMinor(v string) (major, minor int, ok bool) {
	mm := semver.MajorMinor(v)
	if mm == "" {
		return 0, 0, false
	}
	a, b, _ := strings.Cut(strings.TrimPrefix(mm, "v"), ".")
	major, err1 := strconv.Atoi(a)
	minor, err2 := strconv.Atoi(b)
	return major, minor, err1 == nil && err2 == nil
}
