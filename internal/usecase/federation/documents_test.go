package federation

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// allCats is every category, as an owner sees them.
var allCats = domain.ResolveVisibility(domain.VisibilityInput{IsOwner: true})

func TestBuildDelete_MatchesCreateObjectID(t *testing.T) {
	actorID := "https://cairn.example/users/alice"
	objID := actorID + "/activities/019ea3be-f1d5-7b39-8305-26603838c8ee"

	del := BuildDelete(actorID, objID)
	if del["type"] != "Delete" {
		t.Fatalf("type = %v", del["type"])
	}
	// The Delete's object must equal the Create's object id, so the receiving
	// instance's inbound Delete handler removes the matching feed item.
	if del["object"] != objID {
		t.Errorf("Delete object = %v, want %s", del["object"], objID)
	}
	if del["actor"] != actorID {
		t.Errorf("actor = %v", del["actor"])
	}
}

func TestDeliveryBackoff(t *testing.T) {
	cases := map[int]time.Duration{
		1: 30 * time.Second,
		2: 60 * time.Second,
		3: 120 * time.Second,
		4: 240 * time.Second,
	}
	for attempts, want := range cases {
		if got := DeliveryBackoff(attempts); got != want {
			t.Errorf("DeliveryBackoff(%d) = %v, want %v", attempts, got, want)
		}
	}
	// Capped at 6h and never exceeds it, however many attempts.
	for _, a := range []int{12, 20, 100} {
		if got := DeliveryBackoff(a); got != 6*time.Hour {
			t.Errorf("DeliveryBackoff(%d) = %v, want 6h cap", a, got)
		}
	}
	// Monotonic non-decreasing.
	prev := time.Duration(0)
	for a := 1; a <= 12; a++ {
		d := DeliveryBackoff(a)
		if d < prev {
			t.Errorf("backoff decreased at attempt %d: %v < %v", a, d, prev)
		}
		prev = d
	}
}

// The outbound Create we publish (Phase 3) must round-trip through the inbound
// parser (Phase 2) so a remote Cairn surfaces our activity in its home feed.
// This locks the two sides' object shape together.
func TestBuildCreate_RoundTripsThroughInboundParser(t *testing.T) {
	dist := 12345.6
	elev := 210.0
	act := domain.Activity{
		ID:             domain.ActivityID(uuid.New()),
		UserID:         domain.UserID(uuid.New()),
		Type:           domain.ActivityTypeRide,
		Title:          "Morning Ride",
		Description:    "felt strong",
		StartTime:      time.Date(2026, 6, 1, 7, 0, 0, 0, time.UTC),
		MovingDuration: 90 * time.Minute,
		Summary:        domain.ActivitySummary{DistanceM: &dist, ElevationGainM: &elev},
		Privacy:        domain.PrivacyPublic,
	}
	base := "https://cairn.example"
	actorID := base + "/users/alice"
	objID := actorID + "/activities/" + act.ID.String()

	create := BuildCreate(base, actorID, objID, act, allCats)
	if create["type"] != "Create" {
		t.Fatalf("create type = %v, want Create", create["type"])
	}
	if create["actor"] != actorID {
		t.Errorf("actor = %v, want %s", create["actor"], actorID)
	}

	// Marshal the embedded object exactly as it goes on the wire, then feed it
	// to the inbound parser the receiving instance would use.
	objJSON, err := json.Marshal(create["object"])
	if err != nil {
		t.Fatal(err)
	}
	item, ok := FeedItemFromObject(act.UserID, actorID, objJSON)
	if !ok {
		t.Fatal("feedItemFromObject rejected our own Create object")
	}

	if item.Name != "Morning Ride" {
		t.Errorf("name = %q", item.Name)
	}
	if want := base + "/a/" + act.ID.String(); item.URL != want {
		t.Errorf("url = %q, want %q", item.URL, want)
	}
	if want := base + "/api/activities/" + act.ID.String() + "/map.png"; item.ImageURL != want {
		t.Errorf("image = %q, want %q", item.ImageURL, want)
	}
	if item.Sport != "ride" {
		t.Errorf("sport = %q, want ride", item.Sport)
	}
	if item.DistanceM == nil || *item.DistanceM != dist {
		t.Errorf("distance = %v, want %v", item.DistanceM, dist)
	}
	if item.DurationS == nil || *item.DurationS != 5400 {
		t.Errorf("duration = %v, want 5400", item.DurationS)
	}
	if item.ElevationM == nil || *item.ElevationM != elev {
		t.Errorf("elevation = %v, want %v", item.ElevationM, elev)
	}
	if item.Summary != "felt strong" {
		t.Errorf("summary = %q", item.Summary)
	}
	if !item.Published.Equal(act.StartTime) {
		t.Errorf("published = %v, want %v", item.Published, act.StartTime)
	}
}
