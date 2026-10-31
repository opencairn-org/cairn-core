package domain

import "testing"

func TestResolveAudience_Precedence(t *testing.T) {
	cases := []struct {
		in   VisibilityInput
		want Audience
	}{
		{VisibilityInput{IsOwner: true, HasValidLink: true, IsFollower: true}, AudienceOwner},
		{VisibilityInput{HasValidLink: true, IsFollower: true}, AudienceLink},
		{VisibilityInput{IsFollower: true}, AudienceFollowers},
		{VisibilityInput{}, AudiencePublic},
	}
	for i, c := range cases {
		if got := ResolveAudience(c.in); got != c.want {
			t.Errorf("case %d: audience = %s, want %s", i, got, c.want)
		}
	}
}

func TestResolveVisibility_OwnerSeesAll(t *testing.T) {
	got := ResolveVisibility(VisibilityInput{IsOwner: true, OwnerPolicy: DefaultVisibilityPolicy()})
	for _, c := range AllCategories {
		if !got.Has(c) {
			t.Errorf("owner should see %s", c)
		}
	}
}

func TestResolveVisibility_PublicDefaultsToSummaryOnly(t *testing.T) {
	got := ResolveVisibility(VisibilityInput{Privacy: PrivacyPublic, OwnerPolicy: DefaultVisibilityPolicy()})
	if !got.Has(CategorySummary) {
		t.Error("public must see summary")
	}
	for _, c := range []Category{CategoryHR, CategoryPower, CategoryMap, CategoryLocation} {
		if got.Has(c) {
			t.Errorf("public must NOT see %s by default", c)
		}
	}
}

func TestResolveVisibility_FollowerSeesMoreThanPublic(t *testing.T) {
	fol := ResolveVisibility(VisibilityInput{IsFollower: true, Privacy: PrivacyPublic, OwnerPolicy: DefaultVisibilityPolicy()})
	if !fol.Has(CategoryMap) || !fol.Has(CategorySegments) {
		t.Error("follower should see map + segments by default")
	}
	// HR/power still hidden from followers by default.
	if fol.Has(CategoryHR) || fol.Has(CategoryPower) {
		t.Error("follower must not see HR/power by default")
	}
}

func TestResolveVisibility_ActivityOverrideWins(t *testing.T) {
	// Owner default hides HR from followers; this activity opens it up.
	override := &VisibilityPolicy{ByAudience: map[Audience]CategorySet{
		AudienceFollowers: categorySetOf(CategorySummary, CategoryHR),
	}}
	got := ResolveVisibility(VisibilityInput{
		IsFollower: true, Privacy: PrivacyPublic, OwnerPolicy: DefaultVisibilityPolicy(), ActivityPolicy: override,
	})
	if !got.Has(CategoryHR) {
		t.Error("activity override should expose HR to followers")
	}
	if got.Has(CategoryMap) {
		t.Error("override replaces the audience set; map not in override → hidden")
	}
}

func TestResolveVisibility_LinkBypassesPrivate(t *testing.T) {
	// A link holder sees the followers-level set even with an otherwise-locked policy.
	locked := VisibilityPolicy{ByAudience: map[Audience]CategorySet{
		AudiencePublic:    categorySetOf(),
		AudienceFollowers: categorySetOf(),
	}}
	got := ResolveVisibility(VisibilityInput{HasValidLink: true, Privacy: PrivacyPrivate, OwnerPolicy: locked})
	// link falls back to the DEFAULT link set (followers-equivalent) since `locked`
	// doesn't define link.
	if !got.Has(CategorySummary) {
		t.Error("link holder should see at least summary via the default link policy")
	}
}

func TestResolveVisibility_PrivacyAndState(t *testing.T) {
	def := DefaultVisibilityPolicy()
	cases := []struct {
		name string
		in   VisibilityInput
		want bool // any category visible
	}{
		{"public stranger", VisibilityInput{Privacy: PrivacyPublic}, true},
		{"followers-only stranger", VisibilityInput{Privacy: PrivacyFollowers}, false},
		{"followers-only follower", VisibilityInput{Privacy: PrivacyFollowers, IsFollower: true}, true},
		{"followers-only link", VisibilityInput{Privacy: PrivacyFollowers, HasValidLink: true}, true},
		{"private follower", VisibilityInput{Privacy: PrivacyPrivate, IsFollower: true}, false},
		{"private link", VisibilityInput{Privacy: PrivacyPrivate, HasValidLink: true}, true},
		{"unknown privacy fails closed", VisibilityInput{IsFollower: true}, false},
		{"hidden public", VisibilityInput{Privacy: PrivacyPublic, Hidden: true, IsFollower: true}, false},
		{"hidden link", VisibilityInput{Privacy: PrivacyPublic, Hidden: true, HasValidLink: true}, false},
		{"deleted link", VisibilityInput{Privacy: PrivacyPublic, Deleted: true, HasValidLink: true}, false},
		{"owner sees own private", VisibilityInput{Privacy: PrivacyPrivate, IsOwner: true}, true},
	}
	for _, c := range cases {
		c.in.OwnerPolicy = def
		if got := len(ResolveVisibility(c.in)) > 0; got != c.want {
			t.Errorf("%s: visible=%v, want %v", c.name, got, c.want)
		}
	}
}

func TestPrivacyZone_Contains(t *testing.T) {
	// Zone centred on a point with a 200 m radius.
	z := PrivacyZone{Lat: 50.1000, Lng: 10.2000, RadiusM: 200}

	// The exact centre is inside.
	if !z.Contains(50.1000, 10.2000) {
		t.Fatal("centre should be inside the zone")
	}
	// ~50 m north (0.00045° lat ≈ 50 m) is inside.
	if !z.Contains(50.1000+0.00045, 10.2000) {
		t.Error("point ~50 m away should be inside the 200 m zone")
	}
	// ~1 km north is outside.
	if z.Contains(50.1000+0.009, 10.2000) {
		t.Error("point ~1 km away should be outside the 200 m zone")
	}
}

func TestPointInAnyZone(t *testing.T) {
	zones := []PrivacyZone{
		{Lat: 50.1000, Lng: 10.2000, RadiusM: 150}, // zone A
		{Lat: 50.5000, Lng: 10.8000, RadiusM: 300}, // zone B
	}

	// Inside the second zone.
	if !PointInAnyZone(zones, 50.5001, 10.8001) {
		t.Error("point in the second zone should match")
	}
	// Far from both.
	if PointInAnyZone(zones, 52.5200, 13.4050) {
		t.Error("a distant point is in neither zone")
	}
	// Empty zone list never matches (the common no-zones-configured case).
	if PointInAnyZone(nil, 50.1000, 10.2000) {
		t.Error("nil zones must never match")
	}
}
