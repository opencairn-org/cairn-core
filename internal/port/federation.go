package port

import (
	"context"
	"crypto/rsa"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// FederationPublisher announces local activity changes to a user's remote
// ActivityPub followers. Implemented by usecase/federation over the durable delivery
// queue; injected into the primary adapters that change an activity's
// visibility (e.g. the Connect UpdateActivity edit path) so making an activity
// public after import still federates it. A nil publisher is a no-op.
type FederationPublisher interface {
	// PublishCreate announces an activity to the owner's remote followers.
	PublishCreate(ctx context.Context, act domain.Activity)
	// PublishDelete retracts a previously-federated activity from followers.
	PublishDelete(ctx context.Context, userID domain.UserID, activityID domain.ActivityID)
}

// FederationKeyRepo manages the per-user ActivityPub signing keypair. The
// private key is encrypted at rest; only the public PEM (and, for signing,
// the in-memory private key) leave this layer.
type FederationKeyRepo interface {
	// GetOrCreatePublicPEM returns the user's public key in PEM form, lazily
	// generating + persisting a fresh RSA-2048 keypair on first use.
	GetOrCreatePublicPEM(ctx context.Context, userID domain.UserID) (string, error)
	// GetPrivateKey returns the user's decrypted RSA private key for signing
	// outbound activities. Errors if no keypair exists yet.
	GetPrivateKey(ctx context.Context, userID domain.UserID) (*rsa.PrivateKey, error)
}

// FederationActorRepo caches remote actors.
type FederationActorRepo interface {
	Upsert(ctx context.Context, a domain.FederatedActor) error
	Get(ctx context.Context, actorID string) (domain.FederatedActor, error)
}

// FederationFollowRepo persists cross-instance follow edges.
type FederationFollowRepo interface {
	// Upsert inserts or refreshes a follow edge (idempotent on
	// local_user + remote_actor + direction).
	Upsert(ctx context.Context, f domain.FederationFollow) error
	// ListInboundFollowerInboxes returns the distinct inbox URLs (shared inbox
	// preferred) of a local user's accepted remote followers, for fanning out
	// the user's outbound activity Create deliveries.
	ListInboundFollowerInboxes(ctx context.Context, userID domain.UserID) ([]string, error)
	// Exists reports whether a follow edge of the given direction exists.
	Exists(ctx context.Context, userID domain.UserID, remoteActorID string, dir domain.FederationFollowDirection) (bool, error)
	// MarkAccepted flips an edge (e.g. our outbound Follow once the remote Accepts).
	MarkAccepted(ctx context.Context, userID domain.UserID, remoteActorID string, dir domain.FederationFollowDirection) error
	// Delete removes a follow edge — e.g. on an inbound Undo{Follow} (remote
	// unfollows us) or a remote actor self-Delete (defederation).
	Delete(ctx context.Context, userID domain.UserID, remoteActorID string, dir domain.FederationFollowDirection) error
}

// FederationBlockRepo is the instance defederation blocklist (Phase 5).
type FederationBlockRepo interface {
	// Block adds (or refreshes the reason of) a defederated domain.
	Block(ctx context.Context, domain, reason string) error
	// Unblock removes a domain from the blocklist.
	Unblock(ctx context.Context, domain string) error
	// IsBlocked reports whether a domain is defederated.
	IsBlocked(ctx context.Context, domain string) (bool, error)
	// List returns all blocked domains, newest first.
	List(ctx context.Context) ([]domain.BlockedDomain, error)
}

// InboxDedupRepo de-duplicates at-least-once inbound delivery.
type InboxDedupRepo interface {
	// SeenOrMark records the activity id and reports whether it was ALREADY
	// present (i.e. a duplicate delivery to drop).
	SeenOrMark(ctx context.Context, activityID string) (alreadySeen bool, err error)
}

// FederationDeliveryRepo is the durable outbound-delivery queue: each row is a
// signed activity awaiting POST to a remote inbox, retried with backoff.
type FederationDeliveryRepo interface {
	// Enqueue adds a delivery; idempotent on (from_user, inbox, activity_ap_id).
	Enqueue(ctx context.Context, d domain.FederationDelivery) error
	// ListDue returns pending deliveries whose next_attempt_at has elapsed.
	ListDue(ctx context.Context, now time.Time, limit int) ([]domain.FederationDelivery, error)
	// MarkDelivered closes a row as successfully delivered.
	MarkDelivered(ctx context.Context, id domain.FederationDeliveryID) error
	// Reschedule bumps the attempt count + next retry time after a transient failure.
	Reschedule(ctx context.Context, id domain.FederationDeliveryID, attempts int, nextAt time.Time, lastErr string) error
	// MarkDead gives up on a row (max attempts hit, or a permanent reject).
	MarkDead(ctx context.Context, id domain.FederationDeliveryID, lastErr string) error
}

// FederationFeedRepo stores + lists inbound remote activities for a local
// user's home feed.
type FederationFeedRepo interface {
	// Insert adds a received item; idempotent on (recipient, activity_ap_id).
	Insert(ctx context.Context, item domain.FederatedFeedItem) error
	// ListForUser returns the user's federated feed items, newest first.
	ListForUser(ctx context.Context, userID domain.UserID, limit, offset int) ([]domain.FederatedFeedItem, error)
	// DeleteItem removes one item — on an inbound Delete{object} from its author.
	// Scoped by actor so a remote can only delete its own items.
	DeleteItem(ctx context.Context, recipient domain.UserID, actorID, activityAPID string) error
	// DeleteAllFromActor removes every item from one actor — on an actor
	// self-Delete (the remote leaving the network).
	DeleteAllFromActor(ctx context.Context, recipient domain.UserID, actorID string) error
}

// FederationClient speaks ActivityPub over HTTP to remote instances.
type FederationClient interface {
	// FetchActor dereferences a remote actor document (no caching).
	FetchActor(ctx context.Context, actorID string) (domain.FederatedActor, error)
	// ResolveHandle turns user@host into an actor URL via WebFinger.
	ResolveHandle(ctx context.Context, handle string) (string, error)
	// Deliver POSTs a body signed with key to an inbox; status 0 on transport error.
	Deliver(ctx context.Context, key *rsa.PrivateKey, keyID, inboxURL string, body []byte) (int, error)
}

// FederationActivityRepo reads the local activities federation exposes.
type FederationActivityRepo interface {
	GetActivity(ctx context.Context, id domain.ActivityID) (domain.Activity, error)
	// ListPublicActivitiesForUser pages a user's public, non-hidden activities.
	ListPublicActivitiesForUser(ctx context.Context, userID domain.UserID, limit, offset int) ([]domain.Activity, error)
	CountPublicActivitiesForUser(ctx context.Context, userID domain.UserID) (int, error)
}

// RemoteEngagementRepo stores kudos and comments from remote actors.
type RemoteEngagementRepo interface {
	AddRemoteKudos(ctx context.Context, a domain.ActivityID, remoteActorID, likeActivityID string) error
	RemoveRemoteKudos(ctx context.Context, a domain.ActivityID, remoteActorID string) error
	AddRemoteComment(ctx context.Context, a domain.ActivityID, remoteActorID, noteAPID, body string) error
	DeleteRemoteComment(ctx context.Context, remoteActorID, noteAPID string) error
	// DeleteRemoteKudosFromActor / DeleteRemoteCommentsFromActor drop an
	// actor's engagement on one owner's activities.
	DeleteRemoteKudosFromActor(ctx context.Context, ownerUserID domain.UserID, remoteActorID string) error
	DeleteRemoteCommentsFromActor(ctx context.Context, ownerUserID domain.UserID, remoteActorID string) error
}
