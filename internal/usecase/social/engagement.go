package social

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// SocialGate decides whether a viewer may react to an activity (view.Details).
type SocialGate interface {
	SocialAllowed(ctx context.Context, act domain.Activity, me domain.UserID) bool
}

// Engagement gives and takes back kudos and comments.
type Engagement struct {
	Activities port.ActivityRepo
	Repo       port.EngagementRepo
	Gate       SocialGate
}

// Activity loads id when me may interact with it socially.
func (e *Engagement) Activity(ctx context.Context, me domain.UserID, id domain.ActivityID) (domain.Activity, error) {
	a, err := e.Activities.GetActivity(ctx, id)
	if err != nil {
		return domain.Activity{}, domain.ErrNotFound
	}
	if !e.Gate.SocialAllowed(ctx, a, me) {
		return domain.Activity{}, port.NewActionError(port.ActionErrorForbidden, "not permitted")
	}
	return a, nil
}

// AddKudos records me's kudos and returns the new local count.
func (e *Engagement) AddKudos(ctx context.Context, act domain.Activity, me domain.UserID) (int, error) {
	if err := e.Repo.AddKudos(ctx, act.ID, me); err != nil {
		return 0, err
	}
	n, _ := e.Repo.CountKudos(ctx, act.ID)
	return n, nil
}

// RemoveKudos takes back me's kudos and returns the new local count.
func (e *Engagement) RemoveKudos(ctx context.Context, act domain.Activity, me domain.UserID) (int, error) {
	if err := e.Repo.RemoveKudos(ctx, act.ID, me); err != nil {
		return 0, err
	}
	n, _ := e.Repo.CountKudos(ctx, act.ID)
	return n, nil
}

// AddComment stores a trimmed, length-capped comment by me.
func (e *Engagement) AddComment(ctx context.Context, act domain.Activity, me domain.UserID, body string) (domain.CommentID, error) {
	text := strings.TrimSpace(body)
	if text == "" {
		return domain.CommentID{}, port.NewActionError(port.ActionErrorInvalid, "empty comment")
	}
	return e.Repo.AddComment(ctx, domain.Comment{ActivityID: act.ID, UserID: me, Body: TruncateUTF8(text, domain.MaxCommentLength)})
}

// DeleteComment soft-deletes a comment; a no-op unless me wrote it or owns the activity.
func (e *Engagement) DeleteComment(ctx context.Context, me domain.UserID, id domain.CommentID) error {
	return e.Repo.DeleteComment(ctx, id, me)
}

// TruncateUTF8 caps s at maxBytes without splitting a rune (Postgres rejects invalid UTF-8).
func TruncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}
