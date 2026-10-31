package federation

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Actors are the local users as ActivityPub actors.
type Actors struct {
	Instance   Instance
	Users      port.UserRepo
	Keys       port.FederationKeyRepo
	Follows    port.FollowRepo
	Activities port.FederationActivityRepo
	FedFollows port.FederationFollowRepo
	Audience   Audience
	Remote     *Remote
	Sender     *Sender
	Log        *slog.Logger
}

var (
	// ErrNotOptedIn: the caller hasn't enabled federation on their profile.
	ErrNotOptedIn = errors.New("enable federation on your profile first")
	// ErrUnresolvable: the handle has no actor via WebFinger.
	ErrUnresolvable = errors.New("could not resolve handle")
	// ErrRemoteUnreachable: the remote actor document could not be fetched.
	ErrRemoteUnreachable = errors.New("could not fetch remote actor")
)

func (a *Actors) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

// Resolve returns the user iff they opted in to federation.
func (a *Actors) Resolve(ctx context.Context, rawUsername string) (domain.User, bool) {
	u, err := a.Users.GetUserByUsername(ctx, strings.ToLower(strings.TrimSpace(rawUsername)))
	if err != nil {
		return domain.User{}, false
	}
	enabled, err := a.Users.IsFederationEnabled(ctx, u.ID)
	if err != nil || !enabled {
		return domain.User{}, false
	}
	return u, true
}

// PublicKeyPEM is the user's signing key, generated on first use.
func (a *Actors) PublicKeyPEM(ctx context.Context, uid domain.UserID) (string, error) {
	return a.Keys.GetOrCreatePublicPEM(ctx, uid)
}

// Counts are the user's local follower/following counts.
func (a *Actors) Counts(ctx context.Context, uid domain.UserID) domain.FollowCounts {
	c, _ := a.Follows.Counts(ctx, uid)
	return c
}

// OutboxPageSize is the outbox page length.
const OutboxPageSize = 20

// OutboxPage is one page of a user's public activities as Creates.
type OutboxPage struct {
	Total int
	Items []any
	// Fetched counts the rows read, including ones the audience hides.
	Fetched int
}

// OutboxTotal counts the user's public activities.
func (a *Actors) OutboxTotal(ctx context.Context, uid domain.UserID) int {
	n, _ := a.Activities.CountPublicActivitiesForUser(ctx, uid)
	return n
}

// Outbox renders the page at offset with the same Create pushed to
// followers, so a new follower's server can backfill.
func (a *Actors) Outbox(ctx context.Context, u domain.User, offset int) OutboxPage {
	page := OutboxPage{Total: a.OutboxTotal(ctx, u.ID)}
	acts, _ := a.Activities.ListPublicActivitiesForUser(ctx, u.ID, OutboxPageSize, offset)
	page.Fetched = len(acts)
	aid := a.Instance.ActorID(u.Username)
	page.Items = make([]any, 0, len(acts))
	for _, act := range acts {
		cats := publicCats(ctx, a.Audience, act)
		if !cats.Has(domain.CategorySummary) {
			continue
		}
		create := BuildCreate(a.Instance.Base, aid, aid+"/activities/"+act.ID.String(), act, cats)
		delete(create, "@context") // embedded items inherit the page's context
		page.Items = append(page.Items, create)
	}
	return page
}

// SetOptIn toggles the user's federation; enabling generates the keypair
// eagerly so the actor is complete at once.
func (a *Actors) SetOptIn(ctx context.Context, uid domain.UserID, enabled bool) error {
	if err := a.Users.SetFederationEnabled(ctx, uid, enabled); err != nil {
		return err
	}
	if enabled {
		if _, err := a.Keys.GetOrCreatePublicPEM(ctx, uid); err != nil {
			a.log().Error("federation: key gen on enable failed", "user", uid, "error", err)
		}
	}
	return nil
}

// FollowRemote resolves user@host via WebFinger, records a pending follow
// and sends a signed Follow, returning the remote actor URL.
func (a *Actors) FollowRemote(ctx context.Context, uid domain.UserID, handle string) (string, error) {
	if enabled, _ := a.Users.IsFederationEnabled(ctx, uid); !enabled {
		return "", ErrNotOptedIn
	}
	me, err := a.Users.GetUser(ctx, uid)
	if err != nil {
		return "", err
	}
	actorURL, err := a.Remote.ResolveHandle(ctx, handle)
	if err != nil {
		return "", ErrUnresolvable
	}
	remote, _, err := a.Remote.Actor(ctx, actorURL)
	if err != nil {
		return "", ErrRemoteUnreachable
	}
	_ = a.FedFollows.Upsert(ctx, domain.FederationFollow{
		LocalUserID: uid, RemoteActorID: actorURL,
		Direction: domain.FederationFollowOutbound, Status: domain.FederationFollowPending,
	})
	myActor := a.Instance.ActorID(me.Username)
	if err := a.Sender.Send(ctx, uid, myActor, remote.Inbox, buildFollow(myActor, actorURL)); err != nil {
		a.log().Warn("federation: deliver Follow failed", "to", remote.Inbox, "error", err)
	}
	return actorURL, nil
}
