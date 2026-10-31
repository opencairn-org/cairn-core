package view

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
)

func TestActivityAccess(t *testing.T) {
	ctx := context.Background()
	repo := inmem.NewActivityRepo()
	owner, other := domain.UserID(uuid.New()), domain.UserID(uuid.New())
	private := addActivity(repo, owner, domain.ActivityTypeRide, vNow, 1000, domain.PrivacyPrivate)
	deleted := addActivity(repo, owner, domain.ActivityTypeRide, vNow, 1000, domain.PrivacyPublic)
	gone := repo.Activities[deleted]
	now := time.Now()
	gone.DeletedAt = &now
	repo.Activities[deleted] = gone
	a := NewActivityAccess(repo, NewVisibility(nil, nil, nil))

	if _, err := a.Owned(ctx, owner, private); err != nil {
		t.Fatalf("owner: %v", err)
	}
	for name, err := range map[string]error{
		"other's":  errOf(a.Owned(ctx, other, private)),
		"deleted":  errOf(a.Owned(ctx, owner, deleted)),
		"unknown":  errOf(a.Live(ctx, domain.ActivityID(uuid.New()))),
		"live del": errOf(a.Live(ctx, deleted)),
	} {
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, vw, err := a.Visible(ctx, private, &owner, false, domain.CategoryMap); err != nil || !vw.IsOwner {
		t.Fatalf("owner visible: %v %+v", err, vw)
	}
	if _, _, err := a.Visible(ctx, private, &other, false, domain.CategoryMap); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("private to other: %v", err)
	}
	if _, _, err := a.Visible(ctx, private, nil, false, domain.CategoryPhotos); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("private to anonymous: %v", err)
	}
}

func errOf(_ domain.Activity, err error) error { return err }
