// Package match (use-case) orchestrates re-clustering: pull a bucket of source
// records, run the pure matcher + reconciler, apply the plan (reassign sources,
// re-merge affected activities, dissolve emptied ones, record redirects).
package match

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	dmatch "github.com/opencairn-org/cairn-core/internal/domain/match"
	"github.com/opencairn-org/cairn-core/internal/domain/reconcile"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
)

// DefaultBucketMargin is the time-window half-width around an ingested activity.
// ±24h covers the UTC-day boundary and cross-timezone duplicates.
const DefaultBucketMargin = 24 * time.Hour

// ReclusterBucket re-derives the logical-activity grouping for one time bucket.
type ReclusterBucket struct {
	activities port.ActivityRepo
	recompute  *activity.RecomputeActivityFromSources
	denylist   port.SourceDenylistRepo // optional; nil → no detach denylist (Gap 6)
	tx         port.TxManager
	newID      func() uuid.UUID
	opts       dmatch.Options
	logger     *slog.Logger
	events     port.AdminEventSink // optional; receives same-provider conflicts
	lock       port.UserLock       // optional; serialises reclusters of one user
}

// WithUserLock makes each re-cluster hold the user's regrouping lock, so two
// instances never reassign the same bucket from stale snapshots.
func (uc *ReclusterBucket) WithUserLock(l port.UserLock) *ReclusterBucket {
	uc.lock = l
	return uc
}

// WithEvents routes same-provider conflicts to the admin event channel.
func (uc *ReclusterBucket) WithEvents(sink port.AdminEventSink) *ReclusterBucket {
	uc.events = sink
	return uc
}

// NewReclusterBucket wires the use-case. newID/logger default sensibly; opts
// falls back to dmatch.DefaultOptions when zero. denylist may be nil.
func NewReclusterBucket(
	activities port.ActivityRepo,
	recompute *activity.RecomputeActivityFromSources,
	denylist port.SourceDenylistRepo,
	tx port.TxManager,
	newID func() uuid.UUID,
	opts dmatch.Options,
	logger *slog.Logger,
) *ReclusterBucket {
	if newID == nil {
		newID = func() uuid.UUID {
			id, err := uuid.NewV7()
			if err != nil {
				return uuid.New()
			}
			return id
		}
	}
	if opts.EdgeThreshold == 0 && opts.HighBand == 0 {
		opts = dmatch.DefaultOptions()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ReclusterBucket{activities: activities, recompute: recompute, denylist: denylist, tx: tx, newID: newID, opts: opts, logger: logger}
}

// Input selects the bucket: all of a user's source records within ±Margin of
// Around (defaults to DefaultBucketMargin).
type Input struct {
	UserID domain.UserID
	Around time.Time
	Margin time.Duration
}

// Result summarizes what the re-cluster did.
type Result struct {
	AffectedActivityIDs []domain.ActivityID
	SoftDeleted         []domain.ActivityID
	Conflicts           int
}

// Execute re-clusters the bucket transactionally.
func (uc *ReclusterBucket) Execute(ctx context.Context, in Input) (Result, error) {
	margin := in.Margin
	if margin <= 0 {
		margin = DefaultBucketMargin
	}
	from := in.Around.Add(-margin)
	to := in.Around.Add(margin)

	var result Result
	err := uc.tx.InTx(ctx, func(ctx context.Context) error {
		return uc.reclusterInTx(ctx, in.UserID, from, to, &result)
	})
	return result, err
}

// reclusterBucketInput is the matcher view of one bucket.
type reclusterBucketInput struct {
	mrecs            []dmatch.Record
	existing         map[string]string // source id → current activity id
	inBucket         map[string]bool
	deniedSingletons []string // denied sources kept standalone (Gap 6)
}

func (uc *ReclusterBucket) reclusterInTx(ctx context.Context, userID domain.UserID, from, to time.Time, result *Result) error {
	if uc.lock != nil {
		if err := uc.lock.LockUser(ctx, userID); err != nil {
			return err
		}
	}
	records, err := uc.activities.ListSourceRecordsInBucket(ctx, userID, from, to)
	if err != nil {
		return fmt.Errorf("list bucket: %w", err)
	}
	if len(records) <= 1 {
		return nil // nothing to recluster against
	}

	bucket, err := uc.reclusterBuildBucket(ctx, userID, records)
	if err != nil {
		return err
	}

	// Manual constraints scoped to this bucket.
	cons, err := uc.activities.ListMatchConstraints(ctx, userID)
	if err != nil {
		return fmt.Errorf("list constraints: %w", err)
	}
	mc := reclusterConstraints(cons, bucket.inBucket)

	clustered := dmatch.ClusterBucket(bucket.mrecs, mc, uc.opts)
	result.Conflicts = len(clustered.Conflicts)
	for _, cf := range clustered.Conflicts {
		uc.logger.Warn("recluster: same-provider conflict (not merged)",
			"provider", cf.Provider, "a", cf.AID, "b", cf.BID, "score", cf.Score)
		if uc.events != nil {
			uc.events.Publish(domain.NewAdminEvent(domain.AdminEventProviderConflict, domain.AuditWarn, "admin.provider_conflict_detected").
				AboutUser(userID).With("provider", cf.Provider).With("source_a", cf.AID).With("source_b", cf.BID).With("score", fmt.Sprintf("%.2f", cf.Score)))
		}
	}

	// Reconcile clusters → stable activity ids. Denied sources are appended
	// as their own singleton clusters so they stay standalone.
	bandByKey := map[string]dmatch.Band{}
	rcl := make([]reconcile.ClusterInput, 0, len(clustered.Clusters)+len(bucket.deniedSingletons))
	for _, cl := range clustered.Clusters {
		rcl = append(rcl, reconcile.ClusterInput{SourceIDs: cl.RecordIDs})
		bandByKey[clusterKey(cl.RecordIDs)] = cl.Confidence
	}
	for _, sid := range bucket.deniedSingletons {
		rcl = append(rcl, reconcile.ClusterInput{SourceIDs: []string{sid}})
		bandByKey[clusterKey([]string{sid})] = dmatch.BandHigh
	}
	plan := reconcile.Reconcile(rcl, bucket.existing, func() string { return uc.newID().String() })

	bandByActivity, err := uc.reclusterReassign(ctx, plan, bucket.existing, bandByKey)
	if err != nil {
		return err
	}
	if err := uc.reclusterRecompute(ctx, reclusterAffectedIDs(plan, bucket.existing), bandByActivity, result); err != nil {
		return err
	}
	return uc.reclusterRedirects(ctx, plan)
}

// reclusterBuildBucket converts records to matcher input, diverting denied sources.
func (uc *ReclusterBucket) reclusterBuildBucket(ctx context.Context, userID domain.UserID, records []domain.SourceMatchRecord) (reclusterBucketInput, error) {
	b := reclusterBucketInput{
		mrecs:    make([]dmatch.Record, 0, len(records)),
		existing: make(map[string]string, len(records)),
		inBucket: make(map[string]bool, len(records)),
	}
	for _, r := range records {
		sid := r.SourceID.String()
		b.inBucket[sid] = true
		b.existing[sid] = r.ActivityID.String()

		// Gap-6: a previously-detached identity must not auto-merge on
		// re-push — keep it standalone (the denylist survives the new source id).
		if uc.denylist != nil {
			denied, derr := uc.denylist.IsDenied(ctx, userID, r.Provider, r.ExternalAccountID, r.ExternalID)
			if derr != nil {
				return b, fmt.Errorf("denylist check: %w", derr)
			}
			if denied {
				b.deniedSingletons = append(b.deniedSingletons, sid)
				continue
			}
		}

		b.mrecs = append(b.mrecs, dmatch.Record{
			ID:   sid,
			Pool: r.SourceWorkerName,
			SourceFeatures: dmatch.SourceFeatures{
				Provider:   r.Provider,
				SportClass: r.SportClass,
				StartUTC:   r.StartUTC,
				DistanceM:  derefF(r.DistanceM),
				MovingS:    r.MovingS,
				ElapsedS:   r.ElapsedS,
				StartLat:   r.StartLat,
				StartLng:   r.StartLng,
			},
		})
	}
	return b, nil
}

// reclusterConstraints keeps manual constraints whose both ends are in the bucket.
func reclusterConstraints(cons []domain.MatchConstraint, inBucket map[string]bool) dmatch.Constraints {
	var mc dmatch.Constraints
	for _, c := range cons {
		a, b := c.SourceA.String(), c.SourceB.String()
		if !inBucket[a] || !inBucket[b] {
			continue
		}
		switch c.Kind {
		case domain.ConstraintMustLink:
			mc.MustLink = append(mc.MustLink, [2]string{a, b})
		case domain.ConstraintCannotLink:
			mc.CannotLink = append(mc.CannotLink, [2]string{a, b})
		}
	}
	return mc
}

// reclusterReassign moves sources whose activity changed; returns band per activity.
func (uc *ReclusterBucket) reclusterReassign(ctx context.Context, plan reconcile.Plan, existing map[string]string, bandByKey map[string]dmatch.Band) (map[string]dmatch.Band, error) {
	bandByActivity := map[string]dmatch.Band{}
	for _, d := range plan.Decisions {
		bandByActivity[d.ActivityID] = bandByKey[clusterKey(d.SourceIDs)]
		newAct, err := parseActivityID(d.ActivityID)
		if err != nil {
			return nil, err
		}
		for _, sid := range d.SourceIDs {
			if existing[sid] == d.ActivityID {
				continue
			}
			srcID, err := parseSourceID(sid)
			if err != nil {
				return nil, err
			}
			if err := uc.activities.ReassignSource(ctx, srcID, newAct); err != nil {
				return nil, err
			}
		}
	}
	return bandByActivity, nil
}

// reclusterAffectedIDs is every decision target ∪ every old activity, sorted.
func reclusterAffectedIDs(plan reconcile.Plan, existing map[string]string) []string {
	affected := map[string]bool{}
	for _, d := range plan.Decisions {
		affected[d.ActivityID] = true
	}
	for _, old := range existing {
		affected[old] = true
	}
	ids := make([]string, 0, len(affected))
	for a := range affected {
		ids = append(ids, a)
	}
	sort.Strings(ids)
	return ids
}

func (uc *ReclusterBucket) reclusterRecompute(ctx context.Context, ids []string, bandByActivity map[string]dmatch.Band, result *Result) error {
	for _, a := range ids {
		aid, err := parseActivityID(a)
		if err != nil {
			return err
		}
		rec, err := uc.recompute.Execute(ctx, aid)
		if err != nil {
			return fmt.Errorf("recompute %s: %w", a, err)
		}
		if rec.SoftDeleted {
			result.SoftDeleted = append(result.SoftDeleted, aid)
			continue
		}
		if band, ok := bandByActivity[a]; ok {
			if err := uc.activities.SetActivityMatchState(ctx, aid, string(band), band == dmatch.BandMedium); err != nil {
				return err
			}
			result.AffectedActivityIDs = append(result.AffectedActivityIDs, aid)
		}
	}
	return nil
}

// reclusterRedirects records merge redirects so old ids still resolve.
func (uc *ReclusterBucket) reclusterRedirects(ctx context.Context, plan reconcile.Plan) error {
	for _, rd := range plan.Redirects {
		oldID, err := parseActivityID(rd[0])
		if err != nil {
			return err
		}
		newID, err := parseActivityID(rd[1])
		if err != nil {
			return err
		}
		if err := uc.activities.AddActivityRedirect(ctx, oldID, newID, "merge"); err != nil {
			return err
		}
	}
	return nil
}

func clusterKey(ids []string) string { return strings.Join(ids, ",") }

func derefF(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func parseActivityID(s string) (domain.ActivityID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return domain.ActivityID{}, fmt.Errorf("parse activity id %q: %w", s, err)
	}
	return domain.ActivityID(u), nil
}

func parseSourceID(s string) (domain.SourceID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return domain.SourceID{}, fmt.Errorf("parse source id %q: %w", s, err)
	}
	return domain.SourceID(u), nil
}
