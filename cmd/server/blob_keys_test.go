package main

import "testing"

const (
	testUser  = "0190a1b2-0000-7000-8000-000000000001"
	testOther = "0190a1b2-0000-7000-8000-000000000002"
)

func TestParseBlobKey(t *testing.T) {
	cases := map[string]struct {
		want blobKeyInfo
		ok   bool
	}{
		"users/" + testUser + "/strava/sources/s1/b1": {blobKeyInfo{Provider: "strava", UserID: testUser}, true},
		"users/" + testUser + "/garmin/uploads/b1":    {blobKeyInfo{Provider: "garmin", UserID: testUser}, true},
		"shared/polar/b1":                            {blobKeyInfo{Provider: "polar"}, true},
		"transfer/strava/b1.json":                    {blobKeyInfo{Provider: "strava", Transfer: true}, true},
		"users/" + testUser + "/strava/sources/b1":   {ok: false}, // sources needs a source segment
		"users/" + testUser + "/strava/uploads/x/b1": {ok: false},
		"users/not-a-uuid/strava/uploads/b1":         {ok: false},
		"users/" + testUser + "/Strava/uploads/b1":   {ok: false},
		"users/" + testUser + "/strava/../../x":      {ok: false},
		"/users/" + testUser + "/strava/uploads/b1":  {ok: false},
		"exports/u/e.zip":                            {ok: false},
		"transfer/strava":                            {ok: false},
		"":                                           {ok: false},
	}
	for key, c := range cases {
		got, ok := parseBlobKey(key)
		if ok != c.ok || got != c.want {
			t.Errorf("parseBlobKey(%q) = %+v %v, want %+v %v", key, got, ok, c.want, c.ok)
		}
	}
}

func TestCheckOwnedBlob(t *testing.T) {
	own := "users/" + testUser + "/strava/sources/s1/b1"
	if err := checkOwnedBlob("", "strava", testUser); err != nil {
		t.Errorf("empty: %v", err)
	}
	if err := checkOwnedBlob(own, "strava", testUser); err != nil {
		t.Errorf("own: %v", err)
	}
	for name, key := range map[string]string{
		"other user":     "users/" + testOther + "/strava/sources/s1/b1",
		"other provider": "users/" + testUser + "/garmin/sources/s1/b1",
		"transfer":       "transfer/strava/b1.json",
		"shared":         "shared/strava/b1",
		"export":         "exports/" + testUser + "/e.zip",
	} {
		if err := checkOwnedBlob(key, "strava", testUser); err == nil {
			t.Errorf("%s accepted: %q", name, key)
		}
	}
	if err := checkTransferBlob("transfer/strava/b1.json", "strava"); err != nil {
		t.Errorf("transfer own: %v", err)
	}
	for name, key := range map[string]string{
		"other provider": "transfer/garmin/b1.json",
		"raw blob":       own,
		"traversal":      "transfer/strava/../x",
	} {
		if err := checkTransferBlob(key, "strava"); err == nil {
			t.Errorf("%s accepted: %q", name, key)
		}
	}
}
