package federation

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Inbox applies signature-verified inbound activities to a local user.
type Inbox struct {
	Instance   Instance
	Dedup      port.InboxDedupRepo // nil: no de-duplication
	Follows    port.FederationFollowRepo
	Feed       port.FederationFeedRepo
	Engagement port.RemoteEngagementRepo
	Activities port.FederationActivityRepo
	Audience   Audience
	Sender     *Sender
	// Go runs fn in the background; nil uses a plain goroutine.
	Go  func(name string, fn func())
	Log *slog.Logger
}

var (
	// ErrBadActivity is an undecodable activity.
	ErrBadActivity = errors.New("bad activity")
	// ErrWrongTarget is a Follow addressed to someone else.
	ErrWrongTarget = errors.New("wrong target")
)

// Delivery is one authenticated activity addressed to a local user.
type Delivery struct {
	Target        domain.User
	Body          []byte
	RemoteActorID string
	RemoteInbox   string
}

type inboxMsg struct {
	Delivery
	act Activity
}

func (in *Inbox) log() *slog.Logger {
	if in.Log != nil {
		return in.Log
	}
	return slog.Default()
}

// Receive applies one delivery. Unknown types are accepted and ignored.
func (in *Inbox) Receive(ctx context.Context, d Delivery) error {
	var act Activity
	if err := json.Unmarshal(d.Body, &act); err != nil {
		return ErrBadActivity
	}
	// Drop at-least-once duplicates.
	if act.ID != "" && in.Dedup != nil {
		if seen, _ := in.Dedup.SeenOrMark(ctx, act.ID); seen {
			return nil
		}
	}
	m := inboxMsg{Delivery: d, act: act}
	switch act.Type {
	case "Follow":
		// Auto-accept: the user opted in.
		if act.ObjectID() != in.Instance.ActorID(d.Target.Username) {
			return ErrWrongTarget
		}
		in.follow(ctx, m)
	case "Accept":
		// A remote accepted our outbound Follow.
		if err := in.Follows.MarkAccepted(ctx, d.Target.ID, d.RemoteActorID, domain.FederationFollowOutbound); err != nil {
			in.log().Warn("federation: mark outbound accepted", "error", err)
		}
	case "Create":
		in.create(ctx, m)
	case "Like":
		in.like(ctx, m)
	case "Undo":
		in.undo(ctx, m)
	case "Delete":
		in.delete(ctx, m)
	}
	return nil
}

func (in *Inbox) follow(ctx context.Context, m inboxMsg) {
	if err := in.Follows.Upsert(ctx, domain.FederationFollow{
		LocalUserID: m.Target.ID, RemoteActorID: m.RemoteActorID,
		Direction: domain.FederationFollowInbound, Status: domain.FederationFollowAccepted,
		FollowActivityID: m.act.ID,
	}); err != nil {
		in.log().Error("federation: store inbound follow", "error", err)
	}
	accept := func() {
		in.deliverAccept(m.Target.ID, m.act.ID, in.Instance.ActorID(m.Target.Username), m.Body, m.RemoteInbox)
	}
	if in.Go != nil {
		in.Go("federation accept", accept)
	} else {
		go accept()
	}
}

// deliverAccept runs detached from the request so the Accept outlives it.
func (in *Inbox) deliverAccept(targetID domain.UserID, followID, actorID string, raw []byte, inbox string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := in.Sender.Send(ctx, targetID, actorID, inbox, buildAccept(actorID, followID, raw)); err != nil {
		in.log().Warn("federation: deliver Accept failed", "to", inbox, "error", err)
	}
}

// ownsPublic reports whether a is the target's own activity open to the public audience.
func (in *Inbox) ownsPublic(ctx context.Context, a domain.Activity, targetID domain.UserID) bool {
	return a.UserID == targetID && len(publicCats(ctx, in.Audience, a)) > 0
}

// create: a reply to a local public activity becomes a comment; otherwise a
// workout from a followed actor becomes a home-feed item.
func (in *Inbox) create(ctx context.Context, m inboxMsg) {
	noteID, inReplyTo, body, isReply := RemoteComment(m.act.Object)
	if !isReply {
		in.createFeed(ctx, m)
		return
	}
	aid, ok := LocalActivityID(inReplyTo, in.Instance.Host)
	if !ok {
		return
	}
	if act, err := in.Activities.GetActivity(ctx, aid); err == nil && in.ownsPublic(ctx, act, m.Target.ID) {
		body = truncateUTF8(body, domain.MaxCommentLength)
		if err := in.Engagement.AddRemoteComment(ctx, aid, m.RemoteActorID, noteID, body); err != nil {
			in.log().Warn("federation: add remote comment", "error", err)
		}
	}
}

// createFeed ingests workouts only from actors the user follows (strangers are dropped).
func (in *Inbox) createFeed(ctx context.Context, m inboxMsg) {
	following, _ := in.Follows.Exists(ctx, m.Target.ID, m.RemoteActorID, domain.FederationFollowOutbound)
	if !following {
		return
	}
	if item, ok := FeedItemFromObject(m.Target.ID, m.RemoteActorID, m.act.Object); ok {
		if err := in.Feed.Insert(ctx, item); err != nil {
			in.log().Warn("federation: store feed item", "error", err)
		}
	}
}

// like records a federated kudos on the target's own public activity.
func (in *Inbox) like(ctx context.Context, m inboxMsg) {
	aid, ok := LocalActivityID(m.act.ObjectID(), in.Instance.Host)
	if !ok {
		return
	}
	if liked, err := in.Activities.GetActivity(ctx, aid); err == nil && in.ownsPublic(ctx, liked, m.Target.ID) {
		if err := in.Engagement.AddRemoteKudos(ctx, aid, m.RemoteActorID, m.act.ID); err != nil {
			in.log().Warn("federation: add remote kudos", "error", err)
		}
	}
}

func (in *Inbox) undo(ctx context.Context, m inboxMsg) {
	switch m.act.ObjectType() {
	case "Follow":
		// The remote stops following: drop the edge so fan-out stops.
		if m.act.UndoneFollowTarget() == in.Instance.ActorID(m.Target.Username) {
			if err := in.Follows.Delete(ctx, m.Target.ID, m.RemoteActorID, domain.FederationFollowInbound); err != nil {
				in.log().Warn("federation: undo inbound follow", "error", err)
			}
		}
	case "Like":
		if aid, ok := LocalActivityID(m.act.UndoneObject("Like"), in.Instance.Host); ok {
			if err := in.Engagement.RemoveRemoteKudos(ctx, aid, m.RemoteActorID); err != nil {
				in.log().Warn("federation: undo remote kudos", "error", err)
			}
		}
	}
}

// delete removes the signer's own object or, on an actor self-Delete,
// everything of theirs; scoped to the signer.
func (in *Inbox) delete(ctx context.Context, m inboxMsg) {
	switch objID := m.act.ObjectID(); objID {
	case "":
	case m.RemoteActorID:
		if err := in.Feed.DeleteAllFromActor(ctx, m.Target.ID, m.RemoteActorID); err != nil {
			in.log().Warn("federation: delete actor feed items", "error", err)
		}
		_ = in.Engagement.DeleteRemoteKudosFromActor(ctx, m.Target.ID, m.RemoteActorID)
		_ = in.Engagement.DeleteRemoteCommentsFromActor(ctx, m.Target.ID, m.RemoteActorID)
		_ = in.Follows.Delete(ctx, m.Target.ID, m.RemoteActorID, domain.FederationFollowInbound)
		_ = in.Follows.Delete(ctx, m.Target.ID, m.RemoteActorID, domain.FederationFollowOutbound)
	default:
		// A feed activity or a comment Note; whichever matches goes.
		if err := in.Feed.DeleteItem(ctx, m.Target.ID, m.RemoteActorID, objID); err != nil {
			in.log().Warn("federation: delete feed item", "error", err)
		}
		if err := in.Engagement.DeleteRemoteComment(ctx, m.RemoteActorID, objID); err != nil {
			in.log().Warn("federation: delete remote comment", "error", err)
		}
	}
}

// truncateUTF8 cuts s to at most n bytes on a rune boundary.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
