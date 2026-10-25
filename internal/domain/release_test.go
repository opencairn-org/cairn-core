package domain

import "testing"

func TestReleaseCompatible(t *testing.T) {
	for _, c := range []struct {
		core, worker  string
		compat, known bool
	}{
		{"v0.5.0", "v0.5.3", true, true},
		{"v0.5.2-3-gabc123-dirty", "v0.5.0", true, true},
		{"v0.5.0", "v0.4.9", false, true},
		{"v0.5.0", "v0.6.0", false, true}, // pre-1.0: minor must match
		{"v1.5.0", "v0.5.0", false, true},
		{"v1.3.0", "v1.1.4", true, true}, // 1.x: older worker minor works
		{"v1.3.0", "v1.3.9", true, true},
		{"v1.3.0", "v1.4.0", false, true}, // worker needs a newer core
		{"v2.0.0", "v1.9.0", false, true},
		{"dev", "v0.5.0", true, false},
		{"v0.5.0", "", true, false},
	} {
		compat, known := ReleaseCompatible(c.core, c.worker)
		if compat != c.compat || known != c.known {
			t.Errorf("%s/%s = %v,%v", c.core, c.worker, compat, known)
		}
	}
}
