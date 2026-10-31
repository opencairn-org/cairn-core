// Package activity contains use cases that operate on the Activity
// aggregate. The package depends only on internal/domain (types) and
// internal/port (driven interfaces) — no DB, HTTP, or proto imports.
package activity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// RecomputeActivityFromSources is the deterministic re-merge use case.
// It is invoked whenever the set of ActivitySources for an Activity
// changes (new import, reimport, detach, payload preview commit) and
// is safe to re-run idempotently — same source set + same UserSettings
// ⇒ same Activity row.
//
// The use case takes a transaction snapshot of the activity and its
// sources, runs the pure-function merge engine, applies the user-edit
// preservation rules, validates, and persists.
type RecomputeActivityFromSources struct {
	activities    port.ActivityAggregateStore
	settings      port.UserSettingsRepo
	athlete       port.AthleteProfileRepo         // optional; nil → TSS uses engine defaults
	overrides     port.FieldOverrideRepo          // optional; nil → no manual field pins
	classOverride port.ClassificationOverrideRepo // optional; nil → no user classification overlay
	gear          port.GearRepo                   // optional; nil → gear is never derived
	tx            port.TxManager
	now           func() time.Time
}

// NewRecomputeActivityFromSources wires the dependencies. now defaults
// to time.Now when nil — injection is for deterministic tests. athlete may be
// nil (TSS then falls back to engine-default thresholds).
func NewRecomputeActivityFromSources(
	activities port.ActivityAggregateStore,
	settings port.UserSettingsRepo,
	athlete port.AthleteProfileRepo,
	overrides port.FieldOverrideRepo,
	classOverride port.ClassificationOverrideRepo,
	gear port.GearRepo,
	tx port.TxManager,
	now func() time.Time,
) *RecomputeActivityFromSources {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &RecomputeActivityFromSources{
		activities:    activities,
		settings:      settings,
		athlete:       athlete,
		overrides:     overrides,
		classOverride: classOverride,
		gear:          gear,
		tx:            tx,
		now:           now,
	}
}

// RecomputeResult summarises what changed. Callers (typically the
// ingest pipeline) use it to decide whether to dispatch notifications
// and to re-run downstream computations (best efforts, segment matching).
type RecomputeResult struct {
	// Activity is the merged result. Zero-valued when SoftDeleted is true.
	Activity domain.Activity
	// UserID is the owner, set even when SoftDeleted is true.
	UserID domain.UserID

	// SourceIDs lists every non-detached source contributing to Activity.
	SourceIDs []domain.SourceID

	// Created is true when this was the first merge for the activity
	// (no Activity row existed prior).
	Created bool

	// SoftDeleted is true when the activity had no remaining sources
	// and was soft-deleted instead of re-merged.
	SoftDeleted bool

	// GearChanged is true when the activity's gear attachment differs from
	// before (incl. a soft-delete of an activity that had gear).
	GearChanged bool
}

// Execute runs the use case. The activityID must already exist as a row
// in either activities or activity_sources — the caller is responsible
// for creating the activities skeleton (Create+SaveActivity) when ingesting
// a brand new activity before invoking Execute.
//
// Steps:
//
//  1. Read the previous Activity (if any) and its sources atomically.
//  2. If zero sources: soft-delete the activity and return.
//  3. Otherwise: build a PolicyResolver bound to the user, call the
//     pure-function merge engine, then preserve user-controlled fields
//     (Title/Description/Tags/Privacy/GearID/CreatedAt) from the prior row.
//  4. Validate and persist.
func (uc *RecomputeActivityFromSources) Execute(
	ctx context.Context,
	id domain.ActivityID,
) (RecomputeResult, error) {
	var result RecomputeResult

	err := uc.tx.InTx(ctx, func(ctx context.Context) error {
		return uc.recomputeInTx(ctx, id, &result)
	})
	if err == nil && result.GearChanged {
		uc.refreshGearTotals(ctx, result.UserID)
	}
	return result, err
}

// refreshGearTotals rebuilds the user's gear totals after an attachment
// changed. Best-effort and outside the merge transaction: totals are derived
// data, a failure here must not undo a durable merge.
func (uc *RecomputeActivityFromSources) refreshGearTotals(ctx context.Context, userID domain.UserID) {
	if uc.gear == nil || userID == (domain.UserID{}) {
		return
	}
	if err := uc.gear.RecomputeTotalsForUser(ctx, userID); err != nil {
		slog.Warn("gear totals recompute failed", "user_id", userID, "error", err)
	}
}

func (uc *RecomputeActivityFromSources) recomputeInTx(ctx context.Context, id domain.ActivityID, result *RecomputeResult) error {
	// Step 1: snapshot the previous state.
	prev, err := uc.activities.GetActivity(ctx, id)
	prevExists := err == nil
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("get prior activity: %w", err)
	}

	sources, err := uc.activities.ListSourcesForActivity(ctx, id)
	if err != nil {
		return fmt.Errorf("list sources: %w", err)
	}

	// Step 2: zero sources — soft-delete the activity.
	if len(sources) == 0 {
		if !prevExists {
			// Nothing to delete; treat as a no-op success. Common
			// during failed initial ingest cleanup.
			return nil
		}
		if err := uc.activities.SoftDeleteActivity(ctx, id, uc.now()); err != nil {
			return fmt.Errorf("soft delete: %w", err)
		}
		result.SoftDeleted = true
		result.UserID = prev.UserID
		result.GearChanged = prev.GearID != nil
		return nil
	}

	// Step 3: merge, applying any manual per-field source pins (re-read each
	// run so they survive recompute).
	userID := sources[0].UserID
	resolver := uc.buildResolver(ctx, userID)

	overrides, err := uc.loadFieldOverrides(ctx, id)
	if err != nil {
		return err
	}

	merged, err := domain.MergeWithOverrides(id, sources, resolver, overrides, uc.now())
	if err != nil {
		return fmt.Errorf("merge: %w", err)
	}

	var prevPtr *domain.Activity
	if prevExists {
		preserveUserEdits(&merged, prev)
		prevPtr = &prev
	}
	if err := uc.applyOverlays(ctx, id, &merged, prevPtr, sources, overrides, resolver); err != nil {
		return err
	}

	if err := merged.Validate(); err != nil {
		return fmt.Errorf("merge produced invalid activity: %w", err)
	}

	// Step 4: persist.
	if err := uc.activities.SaveActivity(ctx, merged); err != nil {
		return fmt.Errorf("save activity: %w", err)
	}

	result.Activity = merged
	result.UserID = merged.UserID
	result.SourceIDs = collectSourceIDs(sources)
	result.Created = !prevExists
	result.GearChanged = !sameGear(prev.GearID, merged.GearID)
	return nil
}

func sameGear(a, b *domain.GearID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// applyOverlays runs the post-merge steps in order: the user's classification
// overlay (may change Type), TSS estimation, then gear (needs the final type).
func (uc *RecomputeActivityFromSources) applyOverlays(
	ctx context.Context,
	id domain.ActivityID,
	merged, prev *domain.Activity,
	sources []domain.ActivitySource,
	overrides map[domain.FieldGroup]domain.SourceID,
	resolver domain.PolicyResolver,
) error {
	if err := uc.applyClassificationOverride(ctx, id, merged); err != nil {
		return err
	}
	uc.fillEstimatedTSS(ctx, merged)
	return uc.assignGear(ctx, merged, prev, sources, overrides, resolver)
}

// loadFieldOverrides returns the manual per-field source pins (nil when none).
func (uc *RecomputeActivityFromSources) loadFieldOverrides(ctx context.Context, id domain.ActivityID) (map[domain.FieldGroup]domain.SourceID, error) {
	if uc.overrides == nil {
		return nil, nil
	}
	pins, err := uc.overrides.ListForActivity(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("load field overrides: %w", err)
	}
	if len(pins) == 0 {
		return nil, nil
	}
	overrides := make(map[domain.FieldGroup]domain.SourceID, len(pins))
	for _, p := range pins {
		overrides[p.FieldKey] = p.SourceID
	}
	return overrides, nil
}

// applyClassificationOverride applies the user's classification overlay
// (type/discipline/flags) on top of the merge, so a manual correction survives
// re-derivation. Loaded each run; an empty/absent overlay is a no-op.
func (uc *RecomputeActivityFromSources) applyClassificationOverride(ctx context.Context, id domain.ActivityID, merged *domain.Activity) error {
	if uc.classOverride == nil {
		return nil
	}
	co, err := uc.classOverride.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("load classification override: %w", err)
	}
	co.ApplyTo(merged)
	// An override can change Type independently of Discipline; drop a
	// now-incompatible discipline so the result stays valid.
	if !merged.Discipline.CompatibleWith(merged.Type) {
		merged.Discipline = domain.DisciplineNone
	}
	return nil
}

// fillEstimatedTSS derives a training-stress score when no source provided one,
// so the training-load curves have per-activity TSS to integrate. A provider-
// supplied TSS always wins; this only fills the (common) gap. Thresholds
// (FTP / LTHR) are resolved from the athlete's profile AS OF the activity's
// date, so re-computing an old activity uses the values that were true back then.
func (uc *RecomputeActivityFromSources) fillEstimatedTSS(ctx context.Context, merged *domain.Activity) {
	if merged.Summary.TSS != nil {
		return
	}
	th := uc.thresholdsAt(ctx, merged.UserID, merged.StartTime)
	if tss, if_, ok := domain.EstimateTSS(merged.Summary, merged.MovingDuration, th); ok {
		merged.Summary.TSS = &tss
		if merged.Summary.IntensityFactor == nil {
			merged.Summary.IntensityFactor = &if_
		}
	}
}

// thresholdsAt resolves the athlete's FTP / threshold-HR as of the given date.
// Returns the zero value (→ engine defaults) when no profile repo is wired or
// the lookup fails — TSS estimation must always make forward progress.
func (uc *RecomputeActivityFromSources) thresholdsAt(
	ctx context.Context,
	userID domain.UserID,
	at time.Time,
) domain.AthleteThresholds {
	if uc.athlete == nil {
		return domain.AthleteThresholds{}
	}
	entries, err := uc.athlete.ListEntries(ctx, userID)
	if err != nil || len(entries) == 0 {
		return domain.AthleteThresholds{}
	}
	return domain.NewAthleteProfile(entries).ThresholdsAt(at)
}

// buildResolver returns a PolicyResolver that reads from the user's
// configured per-activity-type policy and falls back to the built-in
// defaults if storage is unreachable. The closure captures ctx because
// the resolver is called synchronously from inside the merge engine.
//
// Lookup errors are logged-at-the-adapter-level and swallowed here —
// the merge engine must always make forward progress with SOME policy,
// even if the settings table is temporarily unreadable. The fallback
// is deterministic (same as a brand-new user) so re-running the merge
// once settings storage recovers will produce the canonical result.
func (uc *RecomputeActivityFromSources) buildResolver(
	ctx context.Context,
	userID domain.UserID,
) domain.PolicyResolver {
	return func(t domain.ActivityType) domain.MergePolicy {
		policy, err := uc.settings.GetMergePolicy(ctx, userID, t)
		if err != nil {
			return domain.DefaultMergePolicyFor(t)
		}
		return policy
	}
}

// preserveUserEdits copies the user-controlled fields from a previously-
// persisted Activity onto a freshly-merged one. Once an Activity has been
// created, these fields are owned by the user — subsequent re-merges
// must NOT overwrite them with values from new source imports.
//
// User-controlled fields:
//
//	Title         — user typically renames default provider titles
//	Description   — user-authored notes
//	Tags          — user-assigned labels
//	Privacy       — user-controlled visibility
//	GearID        — user-attached gear
//
// Provenance preservation:
//
//	CreatedAt     — set by the initial insert; never re-written
func preserveUserEdits(merged *domain.Activity, prev domain.Activity) {
	merged.Title = prev.Title
	merged.Description = prev.Description
	merged.Tags = prev.Tags
	merged.Privacy = prev.Privacy
	merged.GearID = prev.GearID
	merged.GearSetBy = prev.GearSetBy
	merged.CreatedAt = prev.CreatedAt
}

// assignGear decides merged.GearID (docs/gear-v0.4.0.md "Assignment"):
//
//   - GearSetBy == user: the user's choice (incl. "no gear") stands.
//   - Otherwise the winning source's provider gear id decides, resolved
//     through the account's gear refs with the policy's gear priority and
//     an optional gear field pin.
//   - No source resolves: a gear that an earlier merge took from a source
//     is dropped (provenance says so); one applied as a default is kept.
//   - First merge of a provider-less activity (manual, upload): the user's
//     default for the type, once.
//
// Runs after the classification overlay so the type default and policy use
// the final type. Without a gear repo nothing is derived.
func (uc *RecomputeActivityFromSources) assignGear(
	ctx context.Context,
	merged *domain.Activity,
	prev *domain.Activity,
	sources []domain.ActivitySource,
	overrides map[domain.FieldGroup]domain.SourceID,
	resolver domain.PolicyResolver,
) error {
	if uc.gear == nil {
		return nil
	}
	if prev != nil && prev.GearSetBy == domain.GearSetByUser {
		merged.GearID, merged.GearSetBy = prev.GearID, domain.GearSetByUser
		return nil
	}
	merged.GearSetBy = domain.GearSetByMerge

	resolved, err := uc.resolveSourceGear(ctx, sources)
	if err != nil {
		return err
	}
	var pin *domain.SourceID
	if sid, ok := overrides[domain.FieldGroupGear]; ok {
		pin = &sid
	}
	if gid, src, ok := domain.ResolveGear(sources, resolved, resolver(merged.Type), pin); ok {
		merged.GearID = &gid
		merged.MergeProvenance.Set(domain.FieldGroupGear, src)
		return nil
	}

	if prev != nil {
		if _, fromSource := prev.MergeProvenance.Winner(domain.FieldGroupGear); fromSource {
			merged.GearID = nil
		} else {
			merged.GearID = prev.GearID
		}
		return nil
	}

	for _, s := range sources {
		if s.ExternalAccountID != nil {
			return nil
		}
	}
	gid, err := uc.gear.GetDefault(ctx, merged.UserID, merged.Type)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("gear default: %w", err)
	}
	merged.GearID = &gid
	return nil
}

// resolveSourceGear maps each source carrying a provider gear id to the Gear
// it resolves to on that account; unresolvable sources are left out.
func (uc *RecomputeActivityFromSources) resolveSourceGear(ctx context.Context, sources []domain.ActivitySource) (map[domain.SourceID]domain.GearID, error) {
	var keys []domain.GearRefKey
	for _, s := range sources {
		if k, ok := domain.GearRefKeyOf(s); ok {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	byKey, err := uc.gear.ResolveExternalRefs(ctx, keys)
	if err != nil {
		return nil, fmt.Errorf("resolve gear refs: %w", err)
	}
	out := make(map[domain.SourceID]domain.GearID, len(byKey))
	for _, s := range sources {
		if k, ok := domain.GearRefKeyOf(s); ok {
			if gid, found := byKey[k]; found {
				out[s.ID] = gid
			}
		}
	}
	return out, nil
}

func collectSourceIDs(sources []domain.ActivitySource) []domain.SourceID {
	ids := make([]domain.SourceID, len(sources))
	for i, s := range sources {
		ids[i] = s.ID
	}
	return ids
}

// PreviewMerge merges the activity's sources with one of them replaced by
// candidate and returns the result WITHOUT persisting anything: the dry run
// behind reimport previews. The gear step is skipped (it may write).
func (uc *RecomputeActivityFromSources) PreviewMerge(ctx context.Context, id domain.ActivityID, candidate domain.ActivitySource) (domain.Activity, error) {
	prev, err := uc.activities.GetActivity(ctx, id)
	if err != nil {
		return domain.Activity{}, fmt.Errorf("get activity: %w", err)
	}
	sources, err := uc.activities.ListSourcesForActivity(ctx, id)
	if err != nil {
		return domain.Activity{}, fmt.Errorf("list sources: %w", err)
	}
	replaced := false
	for i := range sources {
		if sources[i].ID == candidate.ID {
			sources[i] = candidate
			replaced = true
		}
	}
	if !replaced {
		return domain.Activity{}, fmt.Errorf("source %s is not attached to %s: %w", candidate.ID, id, domain.ErrNotFound)
	}
	resolver := uc.buildResolver(ctx, prev.UserID)
	overrides, err := uc.loadFieldOverrides(ctx, id)
	if err != nil {
		return domain.Activity{}, err
	}
	merged, err := domain.MergeWithOverrides(id, sources, resolver, overrides, uc.now())
	if err != nil {
		return domain.Activity{}, fmt.Errorf("merge: %w", err)
	}
	preserveUserEdits(&merged, prev)
	if err := uc.applyClassificationOverride(ctx, id, &merged); err != nil {
		return domain.Activity{}, err
	}
	uc.fillEstimatedTSS(ctx, &merged)
	return merged, nil
}
