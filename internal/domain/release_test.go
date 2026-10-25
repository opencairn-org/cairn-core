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
		{"v1.5.0", "v0.5.0", false, true},
		{"dev", "v0.5.0", true, false},
		{"v0.5.0", "", true, false},
	} {
		compat, known := ReleaseCompatible(c.core, c.worker)
		if compat != c.compat || known != c.known {
			t.Errorf("%s/%s = %v,%v", c.core, c.worker, compat, known)
		}
	}
}
