package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// ShareLinkRepo persists activity share links (migration 34).
type ShareLinkRepo interface {
	Create(ctx context.Context, link domain.ShareLink) error
	// GetActive resolves a non-revoked token. ErrNotFound on miss/revoked.
	GetActive(ctx context.Context, token string) (domain.ShareLink, error)
	ListForActivity(ctx context.Context, activityID domain.ActivityID) ([]domain.ShareLink, error)
	Revoke(ctx context.Context, token string, owner domain.UserID) error
}

// EngagementRepo persists kudos + comments (migration 35).
type EngagementRepo interface {
	AddKudos(ctx context.Context, activityID domain.ActivityID, userID domain.UserID) error
	RemoveKudos(ctx context.Context, activityID domain.ActivityID, userID domain.UserID) error
	CountKudos(ctx context.Context, activityID domain.ActivityID) (int, error)
	HasKudos(ctx context.Context, activityID domain.ActivityID, userID domain.UserID) (bool, error)
	ListKudosers(ctx context.Context, activityID domain.ActivityID, limit int) ([]domain.UserID, error)

	AddComment(ctx context.Context, c domain.Comment) (domain.CommentID, error)
	ListComments(ctx context.Context, activityID domain.ActivityID, limit, offset int) ([]domain.Comment, error)
	// Federated reactions (ActivityPub Likes and reply Notes).
	CountRemoteKudos(ctx context.Context, activityID domain.ActivityID) (int, error)
	ListRemoteKudosers(ctx context.Context, activityID domain.ActivityID, limit int) ([]domain.RemoteKudoser, error)
	ListRemoteComments(ctx context.Context, activityID domain.ActivityID, limit int) ([]domain.RemoteComment, error)
	CountComments(ctx context.Context, activityID domain.ActivityID) (int, error)
	// DeleteComment soft-deletes a comment. Permitted for the comment author
	// OR the owner of the activity the comment is on (enforced in SQL).
	DeleteComment(ctx context.Context, id domain.CommentID, requester domain.UserID) error
}
