package main

import "testing"

func TestSubjectMatches(t *testing.T) {
	cases := []struct {
		filter, subject string
		want            bool
	}{
		{"cairn.jobs.reconcile.garmin", "cairn.jobs.reconcile.garmin", true},
		{"cairn.jobs.reconcile.*", "cairn.jobs.reconcile.garmin", true},
		{"cairn.jobs.>", "cairn.jobs.reconcile.garmin", true},
		{"cairn.jobs.fetch_source.garmin", "cairn.jobs.reconcile.garmin", false},
		{"cairn.jobs.reconcile.garmin.extra", "cairn.jobs.reconcile.garmin", false},
		{"cairn.jobs.reconcile", "cairn.jobs.reconcile.garmin", false},
	}
	for _, c := range cases {
		if got := subjectMatches(c.filter, c.subject); got != c.want {
			t.Errorf("subjectMatches(%q, %q) = %v; want %v", c.filter, c.subject, got, c.want)
		}
	}
}

func TestValidateDiscoverReply(t *testing.T) {
	valid := [][]byte{
		[]byte(`{"items":[{"item_type":"activity","external_id":"1","item_time":"2024-05-06T18:29:24Z"}],"next_page":2}`),
		[]byte(`{"items":[],"complete":true}`),
		[]byte(`{"error":"no credentials for account"}`),
		[]byte(`{"items":[],"rate_limited":true,"next_page":3}`),
	}
	for _, v := range valid {
		if err := validateDiscoverReply(v); err != nil {
			t.Errorf("valid reply rejected: %s → %v", v, err)
		}
	}
	invalid := [][]byte{
		[]byte(`not json`),
		[]byte(`{"unrelated":true}`),
	}
	for _, v := range invalid {
		if err := validateDiscoverReply(v); err == nil {
			t.Errorf("invalid reply accepted: %s", v)
		}
	}
}

func TestReconcileResultSubject_PerPool(t *testing.T) {
	got := reconcileResultSubject("strava", "strava-fetcher", "acct-1")
	if want := "cairn.results.reconcile.strava.strava-fetcher.acct-1"; got != want {
		t.Fatalf("subject = %q, want %q", got, want)
	}
}
