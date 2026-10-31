package sharing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type fakeActivities struct {
	port.ActivityReader
	rows map[domain.ActivityID]domain.Activity
}

func (f *fakeActivities) GetActivity(_ context.Context, id domain.ActivityID) (domain.Activity, error) {
	if a, ok := f.rows[id]; ok {
		return a, nil
	}
	return domain.Activity{}, domain.ErrNotFound
}

type fakeLinks struct {
	port.ShareLinkRepo
	links map[string]domain.ShareLink
}

func (f *fakeLinks) Create(_ context.Context, l domain.ShareLink) error {
	f.links[l.Token] = l
	return nil
}

func (f *fakeLinks) GetActive(_ context.Context, token string) (domain.ShareLink, error) {
	l, ok := f.links[token]
	if !ok || !l.Active() {
		return domain.ShareLink{}, domain.ErrNotFound
	}
	return l, nil
}

func (f *fakeLinks) ListForActivity(_ context.Context, id domain.ActivityID) ([]domain.ShareLink, error) {
	var out []domain.ShareLink
	for _, l := range f.links {
		if l.ActivityID == id {
			out = append(out, l)
		}
	}
	return out, nil
}

// Revoke scopes by owner like the postgres adapter.
func (f *fakeLinks) Revoke(_ context.Context, token string, owner domain.UserID) error {
	l, ok := f.links[token]
	if !ok || l.CreatedBy != owner {
		return domain.ErrNotFound
	}
	now := time.Now()
	l.RevokedAt = &now
	f.links[token] = l
	return nil
}

type fixture struct {
	links             *Links
	repo              *fakeLinks
	owner, other      domain.UserID
	live, gone, alien domain.ActivityID
}

func newFixture() fixture {
	f := fixture{
		owner: domain.UserID(uuid.New()), other: domain.UserID(uuid.New()),
		live: domain.ActivityID(uuid.New()), gone: domain.ActivityID(uuid.New()), alien: domain.ActivityID(uuid.New()),
	}
	deleted := time.Now()
	acts := &fakeActivities{rows: map[domain.ActivityID]domain.Activity{
		f.live:  {ID: f.live, UserID: f.owner},
		f.gone:  {ID: f.gone, UserID: f.owner, DeletedAt: &deleted},
		f.alien: {ID: f.alien, UserID: f.other},
	}}
	f.repo = &fakeLinks{links: map[string]domain.ShareLink{}}
	f.links = &Links{Repo: f.repo, Activities: acts}
	return f
}

func TestCreateListGrantsRevoke(t *testing.T) {
	ctx := context.Background()
	f := newFixture()

	token, err := f.links.Create(ctx, f.owner, f.live)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(token) < 30 {
		t.Errorf("token too short: %q", token)
	}
	if l := f.repo.links[token]; l.ActivityID != f.live || l.CreatedBy != f.owner {
		t.Errorf("stored link: %+v", l)
	}
	if t2, _ := f.links.Create(ctx, f.owner, f.live); t2 == token {
		t.Error("tokens repeat")
	}

	list, err := f.links.List(ctx, f.owner, f.live)
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %v, %v", list, err)
	}

	if !f.links.Grants(ctx, token, f.live) {
		t.Error("active token does not grant its activity")
	}
	if f.links.Grants(ctx, token, f.alien) {
		t.Error("token grants a different activity")
	}
	if f.links.Grants(ctx, "", f.live) || f.links.Grants(ctx, "nope", f.live) {
		t.Error("empty/unknown token granted")
	}

	if err := f.links.Revoke(ctx, f.other, token); err == nil {
		t.Error("non-owner revoked the link")
	}
	if err := f.links.Revoke(ctx, f.owner, token); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if f.links.Grants(ctx, token, f.live) {
		t.Error("revoked token still grants")
	}
}

func TestOwnershipAndDeleted(t *testing.T) {
	ctx := context.Background()
	f := newFixture()
	missing := domain.ActivityID(uuid.New())

	for name, id := range map[string]domain.ActivityID{"foreign": f.alien, "deleted": f.gone, "missing": missing} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.links.Create(ctx, f.owner, id); !errors.Is(err, domain.ErrNotFound) {
				t.Errorf("Create: %v", err)
			}
			if _, err := f.links.List(ctx, f.owner, id); !errors.Is(err, domain.ErrNotFound) {
				t.Errorf("List: %v", err)
			}
		})
	}
	if len(f.repo.links) != 0 {
		t.Error("link created for a non-owned activity")
	}
}

func TestUnavailable(t *testing.T) {
	ctx := context.Background()
	for _, l := range []*Links{nil, {}} {
		if _, err := l.Create(ctx, domain.UserID{}, domain.ActivityID{}); !errors.Is(err, ErrUnavailable) {
			t.Errorf("Create: %v", err)
		}
		if _, err := l.List(ctx, domain.UserID{}, domain.ActivityID{}); !errors.Is(err, ErrUnavailable) {
			t.Errorf("List: %v", err)
		}
		if err := l.Revoke(ctx, domain.UserID{}, "t"); !errors.Is(err, ErrUnavailable) {
			t.Errorf("Revoke: %v", err)
		}
		if l.Grants(ctx, "t", domain.ActivityID{}) {
			t.Error("Grants on unwired links")
		}
	}
}
