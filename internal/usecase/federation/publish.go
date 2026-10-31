package federation

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Publisher pushes a user's public activities to their accepted remote
// followers: one durable delivery per inbox, signed and retried by
// Deliveries. Nil repos disable it; it never fails the caller.
type Publisher struct {
	Enabled    bool
	Instance   Instance
	Users      federatedUserStore
	Keys       port.FederationKeyRepo
	Follows    port.FederationFollowRepo
	Deliveries port.FederationDeliveryRepo // nil: best-effort inline delivery
	Audience   Audience
	Remote     *Remote
	Sender     *Sender
	// Go runs fn in the background; nil uses a plain goroutine.
	Go  func(name string, fn func())
	Log *slog.Logger
}

var _ port.FederationPublisher = (*Publisher)(nil)

func (p *Publisher) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// createEligible: federation on, owner opted in, summary open to the public.
func (p *Publisher) createEligible(ctx context.Context, act domain.Activity) bool {
	if !p.Enabled || p.Follows == nil || p.Keys == nil || p.Users == nil {
		return false
	}
	if !publicCats(ctx, p.Audience, act).Has(domain.CategorySummary) {
		return false
	}
	enabled, err := p.Users.IsFederationEnabled(ctx, act.UserID)
	return err == nil && enabled
}

// PublishCreate announces an activity. The recency gate that keeps a
// backfill from blasting followers lives in the ingest pipeline, so an
// explicit "make public" edit federates an activity of any age.
func (p *Publisher) PublishCreate(ctx context.Context, act domain.Activity) {
	if !p.createEligible(ctx, act) {
		return
	}
	log := p.log()
	inboxes, err := p.Follows.ListInboundFollowerInboxes(ctx, act.UserID)
	if err != nil {
		log.Warn("federation publish: list followers failed", "user_id", act.UserID, "error", err)
		return
	}
	if len(inboxes) == 0 {
		return
	}
	u, err := p.Users.GetUser(ctx, act.UserID)
	if err != nil {
		log.Warn("federation publish: load owner failed", "user_id", act.UserID, "error", err)
		return
	}
	actorID := p.Instance.ActorID(u.Username)
	objID := actorID + "/activities/" + act.ID.String()
	create := BuildCreate(p.Instance.Base, actorID, objID, act, publicCats(ctx, p.Audience, act))
	body, err := json.Marshal(create)
	if err != nil {
		log.Warn("federation publish: marshal Create failed", "activity_id", act.ID, "error", err)
		return
	}
	if p.Deliveries == nil {
		inline := func() { p.publishInline(act, actorID, inboxes, create) }
		if p.Go != nil {
			p.Go("federation publish", inline)
		} else {
			go inline()
		}
		return
	}
	queued := p.enqueue(ctx, act.UserID, actorID, inboxes, body, objID, "federation publish: enqueue failed", act.ID)
	log.Info("federation publish: queued Create deliveries", "activity_id", act.ID, "followers", len(inboxes), "queued", queued)
}

// enqueue queues one delivery per non-blocked inbox and returns the count.
func (p *Publisher) enqueue(ctx context.Context, from domain.UserID, actorID string, inboxes []string, body []byte, apID, failMsg string, actID domain.ActivityID) int {
	queued := 0
	for _, inbox := range inboxes {
		if p.Remote.Blocked(ctx, HostOf(inbox)) {
			continue
		}
		if err := p.Deliveries.Enqueue(ctx, domain.FederationDelivery{
			FromUserID: from, ActorID: actorID, InboxURL: inbox, Body: body, ActivityAPID: apID,
		}); err != nil {
			p.log().Warn(failMsg, "to", inbox, "activity_id", actID, "error", err)
			continue
		}
		queued++
	}
	return queued
}

// publishInline is the fallback without a queue; detached from the caller's context.
func (p *Publisher) publishInline(act domain.Activity, actorID string, inboxes []string, create map[string]any) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, inbox := range inboxes {
		if err := p.Sender.Send(ctx, act.UserID, actorID, inbox, create); err != nil {
			p.log().Warn("federation publish: inline deliver failed", "to", inbox, "activity_id", act.ID, "error", err)
		}
	}
}

// PublishDelete retracts a federated activity (deleted or no longer public);
// the object id matches the Create's, so the receiver drops the feed item.
func (p *Publisher) PublishDelete(ctx context.Context, userID domain.UserID, activityID domain.ActivityID) {
	if !p.Enabled || p.Deliveries == nil || p.Follows == nil || p.Users == nil {
		return
	}
	if enabled, err := p.Users.IsFederationEnabled(ctx, userID); err != nil || !enabled {
		return
	}
	log := p.log()
	inboxes, err := p.Follows.ListInboundFollowerInboxes(ctx, userID)
	if err != nil {
		log.Warn("federation delete: list followers failed", "user_id", userID, "error", err)
		return
	}
	if len(inboxes) == 0 {
		return
	}
	u, err := p.Users.GetUser(ctx, userID)
	if err != nil {
		log.Warn("federation delete: load owner failed", "user_id", userID, "error", err)
		return
	}
	actorID := p.Instance.ActorID(u.Username)
	objID := actorID + "/activities/" + activityID.String()
	body, err := json.Marshal(BuildDelete(actorID, objID))
	if err != nil {
		log.Warn("federation delete: marshal Delete failed", "activity_id", activityID, "error", err)
		return
	}
	// "#delete" keeps the row apart from the Create's for the same (activity, inbox).
	queued := p.enqueue(ctx, userID, actorID, inboxes, body, objID+"#delete", "federation delete: enqueue failed", activityID)
	if queued > 0 {
		log.Info("federation: queued Delete deliveries", "activity_id", activityID, "queued", queued)
	}
}

// Deliveries drains the durable outbound queue: sign + POST each due row,
// back off on transient failures, give up on permanent ones. Replicas
// overlap harmlessly: a double send is a duplicate the receiver dedups.
type Deliveries struct {
	Repo   port.FederationDeliveryRepo
	Remote *Remote
	Sender *Sender
	Log    *slog.Logger
	Now    func() time.Time
}

// deliveryTick is how often due deliveries are drained.
const deliveryTick = 30 * time.Second

func (d *Deliveries) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}

// Run drains the queue every tick until ctx ends.
func (d *Deliveries) Run(ctx context.Context) {
	ticker := time.NewTicker(deliveryTick)
	defer ticker.Stop()
	d.log().Info("federation delivery scheduler started", "interval", deliveryTick.String())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.ProcessDue(ctx)
		}
	}
}

func (d *Deliveries) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// ProcessDue delivers one batch of due rows.
func (d *Deliveries) ProcessDue(ctx context.Context) {
	due, err := d.Repo.ListDue(ctx, d.now(), 50)
	if err != nil {
		d.log().Warn("federation delivery: list due failed", "error", err)
		return
	}
	for _, row := range due {
		d.process(ctx, row)
	}
}

func (d *Deliveries) process(ctx context.Context, row domain.FederationDelivery) {
	log := d.log()
	// Defederation is retroactive: rows queued before the block die.
	if d.Remote.Blocked(ctx, HostOf(row.InboxURL)) {
		_ = d.Repo.MarkDead(ctx, row.ID, "domain blocked")
		return
	}
	code, derr := d.Sender.DeliverBody(ctx, row.FromUserID, row.ActorID, row.InboxURL, row.Body)
	switch {
	case derr == nil && code >= 200 && code < 300:
		if err := d.Repo.MarkDelivered(ctx, row.ID); err != nil {
			log.Warn("federation delivery: mark delivered failed", "id", row.ID, "error", err)
		}
	case derr == nil && code >= 400 && code < 500 && code != 429:
		// Permanent reject (auth, gone, malformed).
		if err := d.Repo.MarkDead(ctx, row.ID, "status "+strconv.Itoa(code)); err != nil {
			log.Warn("federation delivery: mark dead failed", "id", row.ID, "error", err)
		}
		log.Warn("federation delivery: permanent reject", "to", row.InboxURL, "status", code, "activity", row.ActivityAPID)
	default:
		d.retry(ctx, row, code, derr)
	}
}

// retry handles a transient failure (network, 5xx, 429): back off, or give up at the cap.
func (d *Deliveries) retry(ctx context.Context, row domain.FederationDelivery, code int, derr error) {
	log := d.log()
	attempts := row.Attempts + 1
	reason := "status " + strconv.Itoa(code)
	if derr != nil {
		reason = derr.Error()
	}
	if attempts >= row.MaxAttempts {
		if err := d.Repo.MarkDead(ctx, row.ID, reason); err != nil {
			log.Warn("federation delivery: mark dead failed", "id", row.ID, "error", err)
		}
		log.Warn("federation delivery: gave up after max attempts",
			"to", row.InboxURL, "attempts", attempts, "activity", row.ActivityAPID, "reason", reason)
		return
	}
	if err := d.Repo.Reschedule(ctx, row.ID, attempts, d.now().Add(DeliveryBackoff(attempts)), reason); err != nil {
		log.Warn("federation delivery: reschedule failed", "id", row.ID, "error", err)
	}
}

// DeliveryBackoff is capped exponential backoff: 30s, 1m, 2m, … up to 6h.
func DeliveryBackoff(attempts int) time.Duration {
	const base = 30 * time.Second
	const ceiling = 6 * time.Hour
	d := base
	for i := 1; i < attempts && d < ceiling; i++ {
		d *= 2
	}
	return min(d, ceiling)
}
