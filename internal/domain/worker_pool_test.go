package domain

import "testing"

func TestResultSubjectPool(t *testing.T) {
	for subj, want := range map[string][4]string{
		"cairn.results.fetch_source.strava.w1":     {"fetch_source", "strava", "w1", "ok"},
		"cairn.results.fetch_source.strava":        {"fetch_source", "strava", "", "ok"},
		"cairn.results.reconcile.strava.w1.acct-1": {"reconcile", "strava", "w1", "ok"},
		"cairn.results.reconcile.strava.acct-1":    {"reconcile", "strava", "", "ok"},
		"cairn.jobs.fetch_source.strava.w1":        {"", "", "", ""},
	} {
		job, provider, pool, ok := ResultSubjectPool(subj)
		if job != want[0] || provider != want[1] || pool != want[2] || ok != (want[3] == "ok") {
			t.Errorf("%s = %q %q %q %v", subj, job, provider, pool, ok)
		}
	}
}
