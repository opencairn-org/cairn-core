package domain

import (
	"errors"
	"testing"
)

func TestParseParserVersion(t *testing.T) {
	for in, want := range map[string]ParserVersion{
		"1":    {1, 0},
		"1.2":  {1, 2},
		" 2.0": {2, 0},
		"10.4": {10, 4},
	} {
		got, err := ParseParserVersion(in)
		if err != nil || got != want {
			t.Errorf("%q = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "v1", "1.x", "1.2.3", "-1", "1.-2"} {
		if _, err := ParseParserVersion(in); err == nil {
			t.Errorf("%q: want error", in)
		}
	}
}

func TestParserVersion_ReparseRules(t *testing.T) {
	v := func(d, p int) ParserVersion { return ParserVersion{d, p} }
	cases := []struct {
		live, stored ParserVersion
		can          bool
		up           ReparseUpgrade
	}{
		{v(1, 2), v(1, 2), true, UpgradeNone},
		{v(1, 3), v(1, 2), true, UpgradeNewParser},
		{v(1, 1), v(1, 2), false, UpgradeNone}, // would downgrade
		{v(2, 0), v(1, 4), false, UpgradeNewDataset},
		{v(1, 9), v(2, 0), false, UpgradeNone},
	}
	for _, c := range cases {
		if got := c.live.CanReparse(c.stored); got != c.can {
			t.Errorf("%v.CanReparse(%v) = %v", c.live, c.stored, got)
		}
		if got := c.live.UpgradeFor(c.stored); got != c.up {
			t.Errorf("%v.UpgradeFor(%v) = %v", c.live, c.stored, got)
		}
	}
	if v(1, 2).Compare(v(1, 10)) != -1 || v(2, 0).Compare(v(1, 10)) != 1 || v(1, 1).String() != "1.1" {
		t.Error("compare/string")
	}
}

func TestCheckParserForward(t *testing.T) {
	src := ActivitySource{SourceWorkerPackage: "pkg", SourceWorkerVersion: "1.2"}
	for _, c := range []struct {
		pkg, ver string
		reject   bool
	}{
		{"pkg", "1.2", false},
		{"pkg", "1.3", false},
		{"pkg", "2.0", false},
		{"pkg", "1.1", true},
		{"pkg", "1", true},
		{"other", "1.0", false}, // another parser lineage is not compared
		{"pkg", "dev", false},
	} {
		err := CheckParserForward(src, c.pkg, c.ver)
		if got := errors.Is(err, ErrParserDowngrade); got != c.reject {
			t.Errorf("%s %s: err = %v", c.pkg, c.ver, err)
		}
	}
}
