package gear

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
)

type fakeAccounts map[domain.ExternalAccountID]domain.ExternalAccount

func (f fakeAccounts) GetExternalAccount(_ context.Context, id domain.ExternalAccountID) (domain.ExternalAccount, error) {
	a, ok := f[id]
	if !ok {
		return domain.ExternalAccount{}, domain.ErrNotFound
	}
	return a, nil
}

type fakeTx struct{}

func (fakeTx) InTx(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }

type harness struct {
	svc        *Service
	repo       *inmem.GearRepo
	accounts   fakeAccounts
	user       domain.UserID
	recomputed []domain.ActivityID
	notified   []domain.GearExternalRef
}

func newHarness() *harness {
	h := &harness{repo: inmem.NewGearRepo(), accounts: fakeAccounts{}, user: domain.UserID(uuid.New())}
	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	h.svc = NewService(h.repo, h.accounts, fakeTx{},
		func(_ context.Context, id domain.ActivityID) error {
			h.recomputed = append(h.recomputed, id)
			return nil
		},
		func(_ context.Context, ref domain.GearExternalRef) error {
			h.notified = append(h.notified, ref)
			return nil
		},
		func() time.Time { return t0 }, uuid.New)
	return h
}

// account registers an external account of the harness user for provider.
func (h *harness) account(provider string) domain.ExternalAccountID {
	id := domain.ExternalAccountID(uuid.New())
	h.accounts[id] = domain.ExternalAccount{ID: id, UserID: h.user, Provider: provider}
	return id
}

func (h *harness) key(provider string, acct domain.ExternalAccountID, ext string) domain.GearRefKey {
	return domain.GearRefKey{Provider: provider, ExternalAccountID: acct, ExternalID: ext}
}

// importAndCreate records a provider gear and decides "new gear".
func (h *harness) importAndCreate(t *testing.T, key domain.GearRefKey, in domain.GearImport) domain.GearID {
	t.Helper()
	if _, err := h.svc.ImportFromProvider(context.Background(), h.user, key, in); err != nil {
		t.Fatalf("import: %v", err)
	}
	g, err := h.svc.CreateFromRef(context.Background(), h.user, key)
	if err != nil {
		t.Fatalf("create from ref: %v", err)
	}
	return g.ID
}

func TestImportFromProvider_PendingUntilDecided(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	key := h.key("strava", h.account("strava"), "b123")
	km := 1500.0
	in := domain.GearImport{Kind: domain.GearKindBike, Name: "Canyon", Brand: "Canyon", Model: "Grail", Primary: true, DistanceM: &km}

	ref, err := h.svc.ImportFromProvider(ctx, h.user, key, in)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !ref.Pending() || h.repo.Count() != 0 {
		t.Fatalf("unknown provider gear must stay pending, got ref=%+v gear=%d", ref, h.repo.Count())
	}
	if len(h.notified) != 1 || h.notified[0].ProviderName != "Canyon" || h.notified[0].UserID != h.user {
		t.Fatalf("decision notification = %+v", h.notified)
	}
	// Re-sent with the next activity: refreshed, no second notification.
	if _, err := h.svc.ImportFromProvider(ctx, h.user, key, in); err != nil {
		t.Fatal(err)
	}
	if len(h.notified) != 1 {
		t.Fatalf("re-import notified again: %d", len(h.notified))
	}
	if pending, _ := h.svc.ListPending(ctx, h.user); len(pending) != 1 {
		t.Fatalf("pending = %d", len(pending))
	}
	if _, ok, _ := h.svc.DefaultFor(ctx, h.user, domain.ActivityTypeRide); ok {
		t.Fatal("defaults must not be seeded before a decision")
	}

	// The record's activity re-derives on the decision; totals refresh.
	act := domain.ActivityID(uuid.New())
	h.repo.Attach(act, h.user, nil, &key, domain.GearSetByMerge)
	g, err := h.svc.CreateFromRef(ctx, h.user, key)
	if err != nil {
		t.Fatalf("create from ref: %v", err)
	}
	if g.Name != "Canyon" || g.Kind != domain.GearKindBike || g.Brand != "Canyon" || len(g.ExternalRefs) != 1 || !g.ExternalRefs[0].Primary {
		t.Fatalf("created gear = %+v", g)
	}
	if d, ok, _ := h.svc.DefaultFor(ctx, h.user, domain.ActivityTypeRide); !ok || d != g.ID {
		t.Fatalf("ride default = %v/%v", d, ok)
	}
	if len(h.recomputed) != 1 || h.recomputed[0] != act || h.repo.TotalsCalls == 0 {
		t.Fatalf("recomputed=%v totals=%d", h.recomputed, h.repo.TotalsCalls)
	}
	if pending, _ := h.svc.ListPending(ctx, h.user); len(pending) != 0 {
		t.Fatal("still pending after decision")
	}
	if _, err := h.svc.CreateFromRef(ctx, h.user, key); !errors.Is(err, domain.ErrGearRefMapped) {
		t.Fatalf("second decision: %v", err)
	}

	// User renames; the provider re-sends with its own name and a retirement.
	g.Name = "Gravel bike"
	_ = h.repo.Save(ctx, g)
	if _, err := h.svc.ImportFromProvider(ctx, h.user, key, domain.GearImport{Kind: domain.GearKindBike, Name: "Canyon", Retired: true}); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	g, _ = h.repo.Get(ctx, g.ID)
	if g.Name != "Gravel bike" {
		t.Errorf("user name clobbered: %q", g.Name)
	}
	if !g.Retired || g.RetiredAt == nil {
		t.Errorf("provider retirement not followed: %+v", g)
	}
	if h.repo.Count() != 1 {
		t.Errorf("duplicate gear created: %d", h.repo.Count())
	}
}

func TestImportFromProvider_OwnerComesFromTheAccount(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	acct := h.account("strava")

	// Worker claims another user, or the wrong provider, for this account.
	for _, tc := range []struct {
		name string
		user domain.UserID
		key  domain.GearRefKey
	}{
		{"foreign user", domain.UserID(uuid.New()), h.key("strava", acct, "b1")},
		{"wrong provider", h.user, h.key("garmin", acct, "b1")},
	} {
		if _, err := h.svc.ImportFromProvider(ctx, tc.user, tc.key, domain.GearImport{Name: "x"}); !errors.Is(err, ErrNotOwner) {
			t.Fatalf("%s: expected ErrNotOwner, got %v", tc.name, err)
		}
	}
	if _, err := h.svc.ImportFromProvider(ctx, h.user, h.key("strava", domain.ExternalAccountID(uuid.New()), "b1"), domain.GearImport{Name: "x"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown account: %v", err)
	}
	if len(h.notified) != 0 {
		t.Fatalf("rejected imports must not notify: %d", len(h.notified))
	}

	key := h.key("strava", acct, "b1")
	if _, err := h.svc.ImportFromProvider(ctx, h.user, key, domain.GearImport{Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.CreateFromRef(ctx, domain.UserID(uuid.New()), key); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("foreign decision: %v", err)
	}
}

func TestNoteActivityGear_PendingOnlyWhenUnknown(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	key := h.key("strava", h.account("strava"), "b7")

	// Gear id seen only on an activity (import_gear never described it).
	if err := h.svc.NoteActivityGear(ctx, h.user, key, ""); err != nil {
		t.Fatalf("note: %v", err)
	}
	if pending, _ := h.svc.ListPending(ctx, h.user); len(pending) != 1 || len(h.notified) != 1 {
		t.Fatalf("pending = %d, notified = %d", len(pending), len(h.notified))
	}

	// A described ref is not clobbered by the activity's sparse view.
	if _, err := h.svc.ImportFromProvider(ctx, h.user, key, domain.GearImport{Kind: domain.GearKindBike, Name: "Canyon"}); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.NoteActivityGear(ctx, h.user, key, ""); err != nil {
		t.Fatal(err)
	}
	pending, _ := h.svc.ListPending(ctx, h.user)
	if len(pending) != 1 || pending[0].ProviderName != "Canyon" || pending[0].Kind != domain.GearKindBike || len(h.notified) != 1 {
		t.Fatalf("pending = %+v, notified = %d", pending, len(h.notified))
	}

	if err := h.svc.NoteActivityGear(ctx, domain.UserID(uuid.New()), key, ""); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("foreign user: %v", err)
	}
}

func TestDecisions_DefaultNotOverwritten(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	acct := h.account("garmin")
	first := h.importAndCreate(t, h.key("garmin", acct, "1"),
		domain.GearImport{Kind: domain.GearKindShoe, Name: "Pegasus", DefaultFor: []domain.ActivityType{domain.ActivityTypeRun, domain.ActivityTypeWalk}})
	h.importAndCreate(t, h.key("garmin", acct, "2"), domain.GearImport{Kind: domain.GearKindShoe, Name: "Vaporfly", Primary: true})

	if d, _, _ := h.svc.DefaultFor(ctx, h.user, domain.ActivityTypeRun); d != first {
		t.Fatalf("existing run default overwritten by a later primary: %v != %v", d, first)
	}
	if d, _, _ := h.svc.DefaultFor(ctx, h.user, domain.ActivityTypeWalk); d != first {
		t.Fatal("walk default missing")
	}
}

func TestLinkRef_MovesRefAndRederives(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	key := h.key("garmin", h.account("garmin"), "77")
	golden, _ := h.svc.Create(ctx, h.user, CreateInput{Kind: domain.GearKindBike, Name: "Bike A"})
	// Pending record from Garmin: the user decides it is an alias of Bike A.
	if _, err := h.svc.ImportFromProvider(ctx, h.user, key, domain.GearImport{Kind: domain.GearKindBike, Name: "Bike B", Primary: true}); err != nil {
		t.Fatal(err)
	}
	merged := domain.ActivityID(uuid.New())
	userSet := domain.ActivityID(uuid.New())
	h.repo.Attach(merged, h.user, nil, &key, domain.GearSetByMerge)
	h.repo.Attach(userSet, h.user, nil, &key, domain.GearSetByUser)

	g, err := h.svc.LinkRef(ctx, h.user, golden.ID, key)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if len(g.ExternalRefs) != 1 || g.ExternalRefs[0].ExternalID != "77" || g.ExternalRefs[0].ProviderName != "Bike B" {
		t.Fatalf("ref not moved with its fields: %+v", g.ExternalRefs)
	}
	if h.repo.Count() != 1 {
		t.Fatalf("mapping created gear: %d", h.repo.Count())
	}
	if d, _, _ := h.svc.DefaultFor(ctx, h.user, domain.ActivityTypeRide); d != golden.ID {
		t.Fatalf("primary flag did not seed the ride default on mapping: %v", d)
	}
	if len(h.recomputed) != 1 || h.recomputed[0] != merged {
		t.Fatalf("only the merge-assigned activity re-derives: %v", h.recomputed)
	}
	if pending, _ := h.svc.ListPending(ctx, h.user); len(pending) != 0 {
		t.Fatal("mapped ref still pending")
	}
	if _, err := h.svc.LinkRef(ctx, h.user, golden.ID, h.key("strava", domain.ExternalAccountID(uuid.New()), "x")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown ref: %v", err)
	}
	if _, err := h.svc.LinkRef(ctx, domain.UserID(uuid.New()), golden.ID, key); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("foreign user: %v", err)
	}
}

func TestMerge(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	strava := h.importAndCreate(t, h.key("strava", h.account("strava"), "b1"), domain.GearImport{Kind: domain.GearKindBike, Name: "Canyon", Primary: true})
	garmin := h.importAndCreate(t, h.key("garmin", h.account("garmin"), "77"), domain.GearImport{Kind: domain.GearKindBike, Name: "Canyon Grail"})

	if _, err := h.svc.Merge(ctx, h.user, strava, strava); !errors.Is(err, domain.ErrGearSelfMerge) {
		t.Fatalf("self merge: %v", err)
	}
	if _, err := h.svc.Merge(ctx, domain.UserID(uuid.New()), garmin, strava); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("foreign merge: %v", err)
	}
	merged, err := h.svc.Merge(ctx, h.user, garmin, strava)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merged.ID != strava || len(merged.ExternalRefs) != 2 {
		t.Fatalf("merged = %+v", merged)
	}
	if _, err := h.repo.Get(ctx, garmin); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("from still exists: %v", err)
	}
	if d, _, _ := h.svc.DefaultFor(ctx, h.user, domain.ActivityTypeRide); d != strava {
		t.Fatalf("default not repointed: %v", d)
	}
}

func TestDelete_RederivesItsActivities(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	g, _ := h.svc.Create(ctx, h.user, CreateInput{Kind: domain.GearKindBike, Name: "Bike A"})
	act := domain.ActivityID(uuid.New())
	gid := g.ID
	h.repo.Attach(act, h.user, &gid, nil, domain.GearSetByUser)

	if err := h.svc.Delete(ctx, h.user, g.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, setBy := h.repo.ActivityGear(act); got != nil || setBy != domain.GearSetByMerge {
		t.Fatalf("activity after delete: gear=%v set_by=%s", got, setBy)
	}
	if len(h.recomputed) != 1 || h.recomputed[0] != act || h.repo.TotalsCalls == 0 {
		t.Fatalf("recomputed=%v totals=%d", h.recomputed, h.repo.TotalsCalls)
	}
	if err := h.svc.Delete(ctx, h.user, g.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestUpdate_RetireStampsTimeAndValidates(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	g, err := h.svc.Create(ctx, h.user, CreateInput{Kind: domain.GearKindShoe, Name: " Pegasus "})
	if err != nil {
		t.Fatal(err)
	}
	if g.Name != "Pegasus" {
		t.Fatalf("name not trimmed: %q", g.Name)
	}
	g, err = h.svc.Update(ctx, h.user, g.ID, func(g *domain.Gear) { g.Retired = true })
	if err != nil || !g.Retired || g.RetiredAt == nil {
		t.Fatalf("retire: %+v %v", g, err)
	}
	g, _ = h.svc.Update(ctx, h.user, g.ID, func(g *domain.Gear) { g.Retired = false })
	if g.RetiredAt != nil {
		t.Fatal("un-retire kept RetiredAt")
	}
	if _, err := h.svc.Update(ctx, h.user, g.ID, func(g *domain.Gear) { g.Name = "" }); !errors.Is(err, domain.ErrGearMissingName) {
		t.Fatalf("blank name accepted: %v", err)
	}
	if _, err := h.svc.Create(ctx, h.user, CreateInput{Kind: "wheel", Name: "x"}); !errors.Is(err, domain.ErrGearInvalidKind) {
		t.Fatalf("bad kind accepted: %v", err)
	}
	long := make([]byte, domain.GearNameMaxLen+1)
	for i := range long {
		long[i] = 'x'
	}
	if _, err := h.svc.Create(ctx, h.user, CreateInput{Kind: domain.GearKindBike, Name: string(long)}); !errors.Is(err, domain.ErrGearFieldTooLong) {
		t.Fatalf("over-long name accepted: %v", err)
	}
}

func TestSiblingConnections_ShareOneDecision(t *testing.T) {
	// One Garmin account connected for two pools (docs/adr/0006).
	h := newHarness()
	ctx := context.Background()
	apiAcct, fitAcct, otherAcct := h.account("garmin"), h.account("garmin"), h.account("garmin")
	h.repo.SetProviderAccount(apiAcct, "123")
	h.repo.SetProviderAccount(fitAcct, "123")
	h.repo.SetProviderAccount(otherAcct, "456") // another Garmin account: not a sibling
	api, fit, other := h.key("garmin", apiAcct, "g1"), h.key("garmin", fitAcct, "g1"), h.key("garmin", otherAcct, "g1")
	in := domain.GearImport{Kind: domain.GearKindBike, Name: "Canyon"}

	// Both pools report the gear before any decision: two pending refs.
	for _, k := range []domain.GearRefKey{api, fit, other} {
		if _, err := h.svc.ImportFromProvider(ctx, h.user, k, in); err != nil {
			t.Fatal(err)
		}
	}
	act := domain.ActivityID(uuid.New())
	h.repo.Attach(act, h.user, nil, &fit, domain.GearSetByMerge)

	// Deciding on one connection maps the sibling and re-derives its activity.
	g, err := h.svc.CreateFromRef(ctx, h.user, api)
	if err != nil {
		t.Fatal(err)
	}
	if ref, _ := h.repo.GetExternalRef(ctx, fit); ref.GearID == nil || *ref.GearID != g.ID {
		t.Fatalf("sibling ref = %+v", ref)
	}
	if ref, _ := h.repo.GetExternalRef(ctx, other); !ref.Pending() {
		t.Fatalf("another account's ref was mapped: %+v", ref)
	}
	if !slices.Contains(h.recomputed, act) {
		t.Fatalf("sibling activity not re-derived: %v", h.recomputed)
	}

	// A third connection of the same account follows without asking.
	lateAcct := h.account("garmin")
	h.repo.SetProviderAccount(lateAcct, "123")
	notified := len(h.notified)
	ref, err := h.svc.ImportFromProvider(ctx, h.user, h.key("garmin", lateAcct, "g1"), in)
	if err != nil {
		t.Fatal(err)
	}
	if ref.GearID == nil || *ref.GearID != g.ID || len(h.notified) != notified {
		t.Fatalf("late sibling = %+v, notified %d→%d", ref, notified, len(h.notified))
	}
}
