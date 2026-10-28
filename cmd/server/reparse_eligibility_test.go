package main

import (
	"testing"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

func TestReparseWorker(t *testing.T) {
	src := domain.ActivitySource{Provider: "strava", SourceWorkerPackage: "pkg", SourceWorkerVersion: "1.2"}
	w := func(v string, compatible bool) liveWorker {
		return liveWorker{provider: "strava", pkg: "pkg", version: v, compatible: compatible}
	}
	for _, c := range []struct {
		name    string
		workers []liveWorker
		want    string // "" = none
		upgrade string
	}{
		{"same", []liveWorker{w("1.2", true)}, "1.2", ""},
		{"newest wins", []liveWorker{w("1.2", true), w("1.4", true), w("1.3", true)}, "1.4", "new_parser"},
		{"older refused", []liveWorker{w("1.1", true)}, "", ""},
		{"new dataset cannot read", []liveWorker{w("2.0", true)}, "", "new_dataset"},
		{"incompatible release ignored", []liveWorker{w("1.5", false)}, "", ""},
		{"other package ignored", []liveWorker{{provider: "strava", pkg: "x", version: "1.5", compatible: true}}, "", ""},
	} {
		got, ok := reparseWorker(c.workers, src)
		if (c.want == "") == ok || (ok && got.version != c.want) {
			t.Errorf("%s: got %q/%v, want %q", c.name, got.version, ok, c.want)
		}
		if up := domain.SourceUpgrade(domainWorkers(c.workers), src); up != c.upgrade {
			t.Errorf("%s: upgrade %q, want %q", c.name, up, c.upgrade)
		}
	}

	// Legacy bare integer reads as N.0; unparseable versions need an exact match.
	legacy := domain.ActivitySource{Provider: "strava", SourceWorkerPackage: "pkg", SourceWorkerVersion: "1"}
	if got, ok := reparseWorker([]liveWorker{w("1.1", true)}, legacy); !ok || got.version != "1.1" {
		t.Errorf("legacy: %v %v", got, ok)
	}
	odd := domain.ActivitySource{Provider: "strava", SourceWorkerPackage: "pkg", SourceWorkerVersion: "dev"}
	if _, ok := reparseWorker([]liveWorker{w("1.1", true)}, odd); ok {
		t.Error("unparseable source matched a different version")
	}
	if _, ok := reparseWorker([]liveWorker{w("dev", true)}, odd); !ok {
		t.Error("unparseable source did not match its exact version")
	}
}
