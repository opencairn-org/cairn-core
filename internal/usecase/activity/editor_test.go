package activity

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// fedRec records both federation calls.
type fedRec struct{ created, deleted []domain.ActivityID }

func (f *fedRec) PublishCreate(_ context.Context, a domain.Activity) {
	f.created = append(f.created, a.ID)
}

func (f *fedRec) PublishDelete(_ context.Context, _ domain.UserID, id domain.ActivityID) {
	f.deleted = append(f.deleted, id)
}

type memOverlays struct {
	port.ClassificationOverrideRepo
	set map[domain.ActivityID]domain.ClassificationOverride
}

func (m *memOverlays) Get(_ context.Context, id domain.ActivityID) (domain.ClassificationOverride, error) {
	return m.set[id], nil
}

func (m *memOverlays) Set(_ context.Context, o domain.ClassificationOverride) error {
	m.set[o.ActivityID] = o
	return nil
}

// fakeGear owns the gear in owned; def is the type default (nil = none).
type fakeGear struct {
	owned map[domain.GearID]domain.UserID
	def   *domain.GearID
}

func (g *fakeGear) Get(_ context.Context, userID domain.UserID, id domain.GearID) (domain.Gear, error) {
	if g.owned[id] != userID {
		return domain.Gear{}, domain.ErrNotFound
	}
	return domain.Gear{ID: id, UserID: userID}, nil
}

func (g *fakeGear) DefaultFor(context.Context, domain.UserID, domain.ActivityType) (domain.GearID, bool, error) {
	if g.def == nil {
		return domain.GearID{}, false, nil
	}
	return *g.def, true, nil
}

type editorFixture struct {
	uc       *Editor
	repo     *inmem.ActivityRepo
	remerges *remergeSpy
	fed      *fedRec
	events   *eventSpy
	overlays *memOverlays
	gear     *fakeGear
	user     domain.UserID
	act      domain.ActivityID
}

func newEditorFixture(privacy domain.ActivityPrivacy) *editorFixture {
	f := &editorFixture{
		repo: inmem.NewActivityRepo(), remerges: &remergeSpy{}, fed: &fedRec{}, events: &eventSpy{},
		overlays: &memOverlays{set: map[domain.ActivityID]domain.ClassificationOverride{}},
		gear:     &fakeGear{owned: map[domain.GearID]domain.UserID{}},
		user:     domain.UserID(uuid.New()), act: domain.ActivityID(uuid.New()),
	}
	f.repo.Activities[f.act] = domain.Activity{ID: f.act, UserID: f.user, Type: domain.ActivityTypeRide, StartTime: saNow, Privacy: privacy, Title: "old"}
	f.uc = NewEditor(f.repo, nil)
	f.uc.ClassOverrides, f.uc.Gear, f.uc.Federation, f.uc.Events, f.uc.FollowUps = f.overlays, f.gear, f.fed, f.events, f.remerges
	return f
}

func (f *editorFixture) addSource(external bool) {
	sid := domain.SourceID(uuid.New())
	src := domain.ActivitySource{ID: sid, ActivityID: f.act, UserID: f.user, Provider: "manual", Status: domain.SourceStatusActive}
	if external {
		acc := domain.ExternalAccountID(uuid.New())
		src.Provider, src.ExternalAccountID = "strava", &acc
	}
	f.repo.Sources[sid] = src
}

func (f *editorFixture) update(t *testing.T, p domain.ActivityPatch) (domain.Activity, error) {
	t.Helper()
	return f.uc.Update(t.Context(), f.user, f.act, p)
}

func TestEditor_OwnershipAndDeleted(t *testing.T) {
	f := newEditorFixture(domain.PrivacyPrivate)
	if _, err := f.uc.Update(t.Context(), domain.UserID(uuid.New()), f.act, domain.ActivityPatch{Title: new("x")}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign: %v", err)
	}
	a := f.repo.Activities[f.act]
	a.DeletedAt = &saNow
	f.repo.Activities[f.act] = a
	if _, err := f.update(t, domain.ActivityPatch{Title: new("x")}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}
	if f.repo.Activities[f.act].Title != "old" || f.remerges.calls != 0 {
		t.Fatal("rejected edit must not persist")
	}
}

func TestEditor_RejectsEmptyAndMergeManaged(t *testing.T) {
	f := newEditorFixture(domain.PrivacyPrivate)
	if _, err := f.update(t, domain.ActivityPatch{}); !errors.Is(err, ErrEmptyPatch) {
		t.Fatalf("empty: %v", err)
	}
	var ne *FieldNotEditableError
	if _, err := f.update(t, domain.ActivityPatch{Title: new("x"), PrimaryStreamSourceID: &domain.SourceID{}}); !errors.As(err, &ne) || ne.Field != "primary_stream_source_id" {
		t.Fatalf("primary stream: %v", err)
	}
	f.uc.ClassOverrides = nil
	if _, err := f.update(t, domain.ActivityPatch{Overlay: domain.OverlayPatch{IsRace: new(true)}}); !errors.As(err, &ne) || ne.Field != "is_race" {
		t.Fatalf("overlay without store: %v", err)
	}
	if f.repo.Activities[f.act].Title != "old" {
		t.Fatal("rejected edit must not persist")
	}
}

func TestEditor_UserFieldsRemergeAndEvent(t *testing.T) {
	f := newEditorFixture(domain.PrivacyPrivate)
	got, err := f.update(t, domain.ActivityPatch{Title: new("new"), Description: new("d"), Tags: new([]string{"a"})})
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "new" || got.Description != "d" || len(got.Tags) != 1 {
		t.Fatalf("activity %+v", got)
	}
	if f.remerges.calls != 1 {
		t.Fatalf("remerges %d", f.remerges.calls)
	}
	if len(f.events.keys) != 1 || f.events.keys[0] != "activity.updated" {
		t.Fatalf("events %v", f.events.keys)
	}
	if len(f.fed.created)+len(f.fed.deleted) != 0 {
		t.Fatal("no federation without a privacy edit")
	}
}

func TestEditor_FederatesPublicTransitions(t *testing.T) {
	for _, tc := range []struct {
		name             string
		from, to         domain.ActivityPrivacy
		created, deleted int
	}{
		{"go public", domain.PrivacyPrivate, domain.PrivacyPublic, 1, 0},
		{"leave public", domain.PrivacyPublic, domain.PrivacyFollowers, 0, 1},
		{"non-public shuffle", domain.PrivacyFollowers, domain.PrivacyPrivate, 0, 0},
		{"unchanged public", domain.PrivacyPublic, domain.PrivacyPublic, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEditorFixture(tc.from)
			if _, err := f.update(t, domain.ActivityPatch{Privacy: new(tc.to)}); err != nil {
				t.Fatal(err)
			}
			if len(f.fed.created) != tc.created || len(f.fed.deleted) != tc.deleted {
				t.Fatalf("created %v deleted %v", f.fed.created, f.fed.deleted)
			}
		})
	}
}

func TestEditor_GearChoiceIsSticky(t *testing.T) {
	f := newEditorFixture(domain.PrivacyPrivate)
	mine, theirs := domain.GearID(uuid.New()), domain.GearID(uuid.New())
	f.gear.owned[mine], f.gear.owned[theirs] = f.user, domain.UserID(uuid.New())

	got, err := f.update(t, domain.ActivityPatch{Gear: &domain.GearEdit{GearID: &mine}})
	if err != nil {
		t.Fatal(err)
	}
	if got.GearID == nil || *got.GearID != mine || got.GearSetBy != domain.GearSetByUser {
		t.Fatalf("gear %v by %s", got.GearID, got.GearSetBy)
	}
	if _, err := f.update(t, domain.ActivityPatch{Gear: &domain.GearEdit{GearID: &theirs}}); !errors.Is(err, ErrGearNotFound) {
		t.Fatalf("foreign gear: %v", err)
	}
	got, err = f.update(t, domain.ActivityPatch{Gear: &domain.GearEdit{}})
	if err != nil {
		t.Fatal(err)
	}
	if got.GearID != nil || got.GearSetBy != domain.GearSetByUser {
		t.Fatalf("explicit no gear: %v by %s", got.GearID, got.GearSetBy)
	}
	f.uc.Gear = nil
	if _, err := f.update(t, domain.ActivityPatch{Gear: &domain.GearEdit{Reset: true}}); !errors.Is(err, ErrGearUnavailable) {
		t.Fatalf("no gear service: %v", err)
	}
}

func TestEditor_GearResetAppliesDefaultOnlyWhenProviderless(t *testing.T) {
	def := domain.GearID(uuid.New())
	for _, tc := range []struct {
		name     string
		external bool
		want     *domain.GearID
	}{
		{"provider-less gets type default", false, &def},
		{"provider source leaves it to recompute", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEditorFixture(domain.PrivacyPrivate)
			f.gear.def = &def
			f.addSource(tc.external)
			a := f.repo.Activities[f.act]
			a.GearSetBy = domain.GearSetByUser
			f.repo.Activities[f.act] = a

			got, err := f.update(t, domain.ActivityPatch{Gear: &domain.GearEdit{Reset: true}})
			if err != nil {
				t.Fatal(err)
			}
			if got.GearSetBy != domain.GearSetByMerge {
				t.Fatalf("set by %s", got.GearSetBy)
			}
			if (tc.want == nil) != (got.GearID == nil) || (tc.want != nil && *got.GearID != *tc.want) {
				t.Fatalf("gear %v, want %v", got.GearID, tc.want)
			}
		})
	}
}

func TestEditor_OverlayMergesOntoStored(t *testing.T) {
	f := newEditorFixture(domain.PrivacyPrivate)
	run, dist := domain.ActivityTypeRun, 1234.0
	f.overlays.set[f.act] = domain.ClassificationOverride{ActivityID: f.act, Type: &run, IsCommute: new(true)}

	if _, err := f.update(t, domain.ActivityPatch{Overlay: domain.OverlayPatch{
		Type:      &domain.Clearable[domain.ActivityType]{}, // clear → revert to merged
		DistanceM: &domain.Clearable[float64]{Value: &dist},
	}}); err != nil {
		t.Fatal(err)
	}
	co := f.overlays.set[f.act]
	if co.Type != nil || co.IsCommute == nil || !*co.IsCommute || co.DistanceM == nil || *co.DistanceM != dist {
		t.Fatalf("overlay %+v", co)
	}
	if f.remerges.calls != 1 {
		t.Fatal("overlay edit must re-merge")
	}
}
