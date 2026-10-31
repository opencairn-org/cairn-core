package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// federationPublisher adapts the package-level publish funcs to
// port.FederationPublisher, so primary adapters (the Connect edit path) can
// announce visibility changes without depending on package main internals.
type federationPublisher struct {
	app *App
	log *slog.Logger
}

func newFederationPublisher(app *App, log *slog.Logger) *federationPublisher {
	return &federationPublisher{app: app, log: log}
}

var _ port.FederationPublisher = (*federationPublisher)(nil)

func (p *federationPublisher) PublishCreate(ctx context.Context, act domain.Activity) {
	publishActivityCreate(ctx, p.app, p.log, act)
}

func (p *federationPublisher) PublishDelete(ctx context.Context, userID domain.UserID, activityID domain.ActivityID) {
	publishActivityDelete(ctx, p.app, p.log, userID, activityID)
}

// federation_publish.go is ActivityPub Phase 3: OUTBOUND publishing. When a
// local user who has federation enabled gains a fresh, public activity, we
// deliver a signed Create to each of their accepted remote followers' inboxes
// so the workout lands in those followers' home feeds. The object shape mirrors
// what the inbound handler (feedItemFromObject) parses, so Cairn↔Cairn round-
// trips cleanly.
//
// Best-effort fan-out: delivery runs in a detached goroutine and each inbox
// logs-and-continues, so the ingest pipeline never blocks on a slow remote and
// one dead follower can't stall the rest. A durable retry queue is a later
// refinement.

// publishActivityCreate announces a newly-imported activity to the owner's
// remote followers. It is a no-op unless federation is on, the owner opted in,
// and the activity is public + non-hidden. The recency gate that stops a
// historical backfill from blasting followers lives in the ingest pipeline
// (pipeline.FederationMaxAge), NOT here, so an explicit "make public" edit
// federates an activity of any age.
func publishActivityCreate(ctx context.Context, app *App, log *slog.Logger, act domain.Activity) {
	if !publishCreateEligible(ctx, app, act) {
		return
	}
	inboxes, err := app.FederationFollows.ListInboundFollowerInboxes(ctx, act.UserID)
	if err != nil {
		log.Warn("federation publish: list followers failed", "user_id", act.UserID, "error", err)
		return
	}
	if len(inboxes) == 0 {
		return // nobody to deliver to
	}
	u, err := app.Users.GetUser(ctx, act.UserID)
	if err != nil {
		log.Warn("federation publish: load owner failed", "user_id", act.UserID, "error", err)
		return
	}

	base := strings.TrimRight(app.PublicBaseURL, "/")
	actorID := base + "/users/" + u.Username
	objID := actorID + "/activities/" + act.ID.String()
	create := buildActivityCreate(base, actorID, objID, act, fedPublicCats(ctx, app, act))
	body, err := json.Marshal(create)
	if err != nil {
		log.Warn("federation publish: marshal Create failed", "activity_id", act.ID, "error", err)
		return
	}

	// Enqueue one durable delivery per follower inbox; the delivery scheduler
	// signs + POSTs and retries transient failures with backoff. Falls back to
	// best-effort inline delivery only if the queue isn't wired.
	if app.FederationDeliveries == nil {
		goSafe(log, "federation publish", func() { publishCreateInline(app, log, act, actorID, inboxes, create) })
		return
	}
	queued := 0
	for _, inbox := range inboxes {
		// Defederation: never deliver to a blocked domain.
		if domainBlocked(ctx, app, hostOf(inbox)) {
			continue
		}
		if err := app.FederationDeliveries.Enqueue(ctx, domain.FederationDelivery{
			FromUserID: act.UserID, ActorID: actorID, InboxURL: inbox,
			Body: body, ActivityAPID: objID,
		}); err != nil {
			log.Warn("federation publish: enqueue failed", "to", inbox, "activity_id", act.ID, "error", err)
			continue
		}
		queued++
	}
	log.Info("federation publish: queued Create deliveries",
		"activity_id", act.ID, "followers", len(inboxes), "queued", queued)
}

// publishCreateEligible: federation on, owner opted in, summary open to the public audience.
func publishCreateEligible(ctx context.Context, app *App, act domain.Activity) bool {
	if !app.FederationEnabled || app.FederationFollows == nil || app.FederationKeys == nil || app.Users == nil {
		return false
	}
	if !fedPublicCats(ctx, app, act).Has(domain.CategorySummary) {
		return false
	}
	enabled, err := app.Users.IsFederationEnabled(ctx, act.UserID)
	return err == nil && enabled
}

// fedPublicCats is what an anonymous viewer may see of act; federated copies
// are public, so they never carry more.
func fedPublicCats(ctx context.Context, app *App, act domain.Activity) domain.CategorySet {
	if app.Visibility2 == nil {
		return domain.CategorySet{}
	}
	return app.Visibility2.For(ctx, &act, nil, false)
}

// publishCreateInline is the best-effort fallback; detached from the caller's context.
func publishCreateInline(app *App, log *slog.Logger, act domain.Activity, actorID string, inboxes []string, create map[string]any) {
	dctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, inbox := range inboxes {
		if err := deliverActivity(dctx, app, act.UserID, actorID, inbox, create); err != nil {
			log.Warn("federation publish: inline deliver failed", "to", inbox, "activity_id", act.ID, "error", err)
		}
	}
}

// publishActivityDelete tells the owner's remote followers that a federated
// activity is gone (deleted or no longer public). The Delete's object id matches
// the Create's object id, so a receiving Cairn's inbound Delete handler removes
// the feed item. No-op unless federation is on + the owner opted in + has
// followers; best-effort via the durable queue.
func publishActivityDelete(ctx context.Context, app *App, log *slog.Logger, userID domain.UserID, activityID domain.ActivityID) {
	if !app.FederationEnabled || app.FederationDeliveries == nil || app.FederationFollows == nil || app.Users == nil {
		return
	}
	if enabled, err := app.Users.IsFederationEnabled(ctx, userID); err != nil || !enabled {
		return
	}
	inboxes, err := app.FederationFollows.ListInboundFollowerInboxes(ctx, userID)
	if err != nil {
		log.Warn("federation delete: list followers failed", "user_id", userID, "error", err)
		return
	}
	if len(inboxes) == 0 {
		return
	}
	u, err := app.Users.GetUser(ctx, userID)
	if err != nil {
		log.Warn("federation delete: load owner failed", "user_id", userID, "error", err)
		return
	}
	base := strings.TrimRight(app.PublicBaseURL, "/")
	actorID := base + "/users/" + u.Username
	objID := actorID + "/activities/" + activityID.String()
	del := buildActivityDelete(actorID, objID)
	body, err := json.Marshal(del)
	if err != nil {
		log.Warn("federation delete: marshal Delete failed", "activity_id", activityID, "error", err)
		return
	}
	queued := 0
	for _, inbox := range inboxes {
		if domainBlocked(ctx, app, hostOf(inbox)) {
			continue
		}
		// "#delete" namespaces the delivery row so it doesn't collide with the
		// original Create's row for the same (activity, inbox).
		if err := app.FederationDeliveries.Enqueue(ctx, domain.FederationDelivery{
			FromUserID: userID, ActorID: actorID, InboxURL: inbox,
			Body: body, ActivityAPID: objID + "#delete",
		}); err != nil {
			log.Warn("federation delete: enqueue failed", "to", inbox, "activity_id", activityID, "error", err)
			continue
		}
		queued++
	}
	if queued > 0 {
		log.Info("federation: queued Delete deliveries", "activity_id", activityID, "queued", queued)
	}
}

// buildActivityDelete is a minimal AS2 Delete addressed to the actor's followers.
func buildActivityDelete(actorID, objID string) map[string]any {
	return map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       objID + "#delete",
		"type":     "Delete",
		"actor":    actorID,
		"to":       []string{"https://www.w3.org/ns/activitystreams#Public"},
		"cc":       []string{actorID + "/followers"},
		"object":   objID,
	}
}

// federationDeliveryTick is how often the scheduler drains due deliveries.
const federationDeliveryTick = 30 * time.Second

// runFederationDeliveryScheduler drains the durable outbound delivery queue:
// it claims due rows, signs + POSTs each, and on failure reschedules with
// capped exponential backoff (transient) or marks the row dead (max attempts
// or a permanent 4xx reject). One per process; the claim query orders by
// next_attempt_at so replicas overlap harmlessly (a double-send is just a
// duplicate the receiver dedups).
func runFederationDeliveryScheduler(ctx context.Context, logger *slog.Logger, app *App) {
	log := logger.With("component", "federation_delivery")
	ticker := time.NewTicker(federationDeliveryTick)
	defer ticker.Stop()
	log.Info("federation delivery scheduler started", "interval", federationDeliveryTick.String())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			processFederationDeliveries(ctx, log, app)
		}
	}
}

func processFederationDeliveries(ctx context.Context, log *slog.Logger, app *App) {
	due, err := app.FederationDeliveries.ListDue(ctx, time.Now().UTC(), 50)
	if err != nil {
		log.Warn("federation delivery: list due failed", "error", err)
		return
	}
	for _, d := range due {
		processFederationDelivery(ctx, log, app, d)
	}
}

func processFederationDelivery(ctx context.Context, log *slog.Logger, app *App, d domain.FederationDelivery) {
	// Defederation is retroactive: a domain blocked AFTER this row was
	// queued must not still receive the activity. Kill in-flight deliveries
	// to a now-blocked inbox.
	if domainBlocked(ctx, app, hostOf(d.InboxURL)) {
		_ = app.FederationDeliveries.MarkDead(ctx, d.ID, "domain blocked")
		return
	}
	code, derr := deliverSignedBody(ctx, app, d.FromUserID, d.ActorID, d.InboxURL, d.Body)
	switch {
	case derr == nil && code >= 200 && code < 300:
		if err := app.FederationDeliveries.MarkDelivered(ctx, d.ID); err != nil {
			log.Warn("federation delivery: mark delivered failed", "id", d.ID, "error", err)
		}
	case derr == nil && code >= 400 && code < 500 && code != 429:
		// Permanent reject (auth, gone, malformed) — stop retrying.
		if err := app.FederationDeliveries.MarkDead(ctx, d.ID, "status "+itoa(code)); err != nil {
			log.Warn("federation delivery: mark dead failed", "id", d.ID, "error", err)
		}
		log.Warn("federation delivery: permanent reject", "to", d.InboxURL, "status", code, "activity", d.ActivityAPID)
	default:
		processFederationDeliveryRetry(ctx, log, app, d, code, derr)
	}
}

// processFederationDeliveryRetry handles a transient failure (network, 5xx, 429): back off, or give up at the cap.
func processFederationDeliveryRetry(ctx context.Context, log *slog.Logger, app *App, d domain.FederationDelivery, code int, derr error) {
	attempts := d.Attempts + 1
	reason := "status " + itoa(code)
	if derr != nil {
		reason = derr.Error()
	}
	if attempts >= d.MaxAttempts {
		if err := app.FederationDeliveries.MarkDead(ctx, d.ID, reason); err != nil {
			log.Warn("federation delivery: mark dead failed", "id", d.ID, "error", err)
		}
		log.Warn("federation delivery: gave up after max attempts",
			"to", d.InboxURL, "attempts", attempts, "activity", d.ActivityAPID, "reason", reason)
		return
	}
	next := time.Now().UTC().Add(deliveryBackoff(attempts))
	if err := app.FederationDeliveries.Reschedule(ctx, d.ID, attempts, next, reason); err != nil {
		log.Warn("federation delivery: reschedule failed", "id", d.ID, "error", err)
	}
}

// deliveryBackoff is capped exponential backoff: 30s, 1m, 2m, … up to 6h.
func deliveryBackoff(attempts int) time.Duration {
	const base = 30 * time.Second
	const cap = 6 * time.Hour
	d := base
	for i := 1; i < attempts && d < cap; i++ {
		d *= 2
	}
	if d > cap {
		d = cap
	}
	return d
}

func itoa(n int) string { return strconv.Itoa(n) }

// buildActivityCreate assembles the Create activity + its embedded object. The
// object's `sport:summary` block + `image` are exactly what feedItemFromObject
// reads on the receiving side. The map image is only linked when cats has it.
func buildActivityCreate(base, actorID, objID string, act domain.Activity, cats domain.CategorySet) map[string]any {
	sport := map[string]any{"discipline": string(act.Type)}
	if act.Summary.DistanceM != nil {
		sport["distance_m"] = *act.Summary.DistanceM
	}
	if act.MovingDuration > 0 {
		sport["moving_s"] = int(act.MovingDuration.Seconds())
	}
	if act.Summary.ElevationGainM != nil {
		sport["elevation_gain_m"] = *act.Summary.ElevationGainM
	}
	title := act.Title
	if title == "" {
		title = "Activity"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	followers := actorID + "/followers"
	public := []string{"https://www.w3.org/ns/activitystreams#Public"}
	object := map[string]any{
		"id":            objID,
		"type":          "Note",
		"attributedTo":  actorID,
		"name":          title,
		"content":       act.Description,
		"url":           base + "/a/" + act.ID.String(),
		"published":     act.StartTime.UTC().Format(time.RFC3339),
		"sport:summary": sport,
	}
	if cats.Has(domain.CategoryMap) {
		object["image"] = base + "/api/activities/" + act.ID.String() + "/map.png"
	}
	return map[string]any{
		"@context":  "https://www.w3.org/ns/activitystreams",
		"id":        objID + "#create",
		"type":      "Create",
		"actor":     actorID,
		"published": now,
		"to":        public,
		"cc":        []string{followers},
		"object":    object,
	}
}
