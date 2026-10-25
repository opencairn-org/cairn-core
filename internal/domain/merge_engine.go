package domain

import (
	"sort"
	"time"
)

// ---------------------------------------------------------------------------
// Merge engine
//
// The merge engine assembles an Activity from one or more ActivitySources.
// It is a pure function: given the same (sources, resolver) inputs it
// always produces the same Activity, including the same MergeProvenance.
// This makes RecomputeActivityFromSources safe to re-run after a source
// is added, replaced, deleted, or reimported.
//
// Algorithm
// ---------
//
//  1. Pre-sort sources by ImportedAt DESC. Within a provider the most
//     recent import wins ties; across providers the priority list decides.
//
//  2. Resolve FieldGroupClassification first using a bootstrap policy
//     (looked up against the hint type — the most-recently-imported
//     source's reported type). Classification picks Type, Discipline,
//     and the boolean flags.
//
//  3. With the now-known Type, look up the real per-type MergePolicy
//     and resolve the remaining field groups in AllFieldGroups order.
//     Each group's winner has its ApplyTo invoked on a zero Activity.
//
//  4. For FieldGroupGPSTrack and FieldGroupLaps the winner does not
//     write any summary fields. Instead the engine sets
//     PrimaryStreamSourceID and records provenance for the laps source
//     (which the persistence layer reads from when materializing laps).
//
// Invariants
// ----------
//
//   * No source set => ErrNoSources.
//   * Sources from different users => ErrSourcesMismatchedUser.
//   * No source provides a valid classification => ErrNoClassification.
//
// The caller (RecomputeActivityFromSources use case) is responsible for
// supplying source records that all belong to the same Activity. Mixed
// activity_ids are not detected here; the merge engine treats the input
// as the authoritative set.
// ---------------------------------------------------------------------------

// PolicyResolver returns the merge policy applicable to a given ActivityType.
// Implementations typically read UserSettings.MergePolicyByActivityType and
// fall back to DefaultMergePolicyFor when the user has not overridden.
type PolicyResolver func(ActivityType) MergePolicy

// Merge runs the merge engine. activityID is preserved (re-merges on an
// existing activity), the user is derived from the sources, and now is
// the timestamp written to Activity.MergedAt (passed in for testability).
//
// The returned Activity has all merged fields populated but NOT:
//   - Title and Description from user edits (callers apply those after)
//   - Tags, Privacy, GearID (user-controlled)
//   - CreatedAt, UpdatedAt, DeletedAt (persistence-managed)
//
// Validate the result before persisting.
func Merge(
	activityID ActivityID,
	sources []ActivitySource,
	resolver PolicyResolver,
	now time.Time,
) (Activity, error) {
	return MergeWithOverrides(activityID, sources, resolver, nil, now)
}

// MergeWithOverrides is Merge plus manual per-field-group source pins: a pinned
// source wins its group over the cascade (ignored if it's absent or doesn't
// provide the group).
func MergeWithOverrides(
	activityID ActivityID,
	sources []ActivitySource,
	resolver PolicyResolver,
	overrides map[FieldGroup]SourceID,
	now time.Time,
) (Activity, error) {
	if len(sources) == 0 {
		return Activity{}, ErrNoSources
	}

	userID := sources[0].UserID
	for _, s := range sources[1:] {
		if s.UserID != userID {
			return Activity{}, ErrSourcesMismatchedUser
		}
	}

	// Work on a defensive copy — sort mutates the slice header.
	ordered := make([]ActivitySource, len(sources))
	copy(ordered, sources)
	sort.SliceStable(ordered, func(i, j int) bool {
		// Most recent first; stable sort preserves any caller-provided
		// secondary order (e.g. by provider name) for ties.
		return ordered[i].ImportedAt.After(ordered[j].ImportedAt)
	})

	out := Activity{
		ID:              activityID,
		UserID:          userID,
		MergeProvenance: MergeProvenance{},
		MergedAt:        now,
		Privacy:         PrivacyPrivate, // safe default; overwritten if existing activity has user pref
	}

	// ----- 1. Classification (bootstrap policy) ------------------------

	// Use the most-recently-imported source's reported type as a hint
	// to fetch the bootstrap policy. The resolver is allowed to return
	// the same MergePolicy for every type — DefaultMergePolicyFor for
	// example sets identical classification overrides.
	hintType := ordered[0].Parsed.Type
	bootstrap := resolver(hintType)

	classificationWinner, ok := overrideWinner(ordered, FieldGroupClassification, overrides)
	if !ok {
		classificationWinner, ok = pickWinner(ordered, FieldGroupClassification, bootstrap)
	}
	if !ok {
		return Activity{}, ErrNoClassification
	}
	classificationWinner.Parsed.ApplyTo(FieldGroupClassification, &out)
	out.MergeProvenance.Set(FieldGroupClassification, classificationWinner.ID)

	// ----- 2. Per-type policy for remaining groups ---------------------

	policy := resolver(out.Type)

	mergeApplyGroups(&out, ordered, policy, overrides)

	// ----- 3. Derived invariants ---------------------------------------

	mergeDeriveInvariants(&out)

	// Tags default to empty (user-managed). The merge engine does not
	// union source tags — that decision belongs to the use case, which
	// can read the previous Activity's tag set and preserve it.

	return out, nil
}

// overrideWinner returns the pinned source for group g if it's present and
// provides the group; else (zero, false) so the caller uses the cascade.
func overrideWinner(sources []ActivitySource, g FieldGroup, overrides map[FieldGroup]SourceID) (ActivitySource, bool) {
	if len(overrides) == 0 {
		return ActivitySource{}, false
	}
	pinned, ok := overrides[g]
	if !ok {
		return ActivitySource{}, false
	}
	for _, s := range sources {
		if s.ID == pinned && s.Parsed.Provides(g) {
			return s, true
		}
	}
	return ActivitySource{}, false
}

// pickWinner returns the highest-priority source that provides field
// group g per the supplied policy. Sources must be pre-sorted by
// ImportedAt DESC; ties within a provider go to the most recent import.
//
// Priority semantics:
//
//   - Specific provider names match sources from that provider only.
//   - AnyProvider matches sources whose provider is NOT explicitly named
//     elsewhere in this priority list — so ["garmin", "_any"] means
//     "Garmin first, then any source whose provider is not Garmin".
//
// Returns (zero ActivitySource, false) when no source matches.
func pickWinner(sources []ActivitySource, g FieldGroup, policy MergePolicy) (ActivitySource, bool) {
	return pickWinnerBy(sources, policy.PriorityFor(g), func(s *ActivitySource) bool {
		return s.Parsed.Provides(g)
	})
}

// pickWinnerBy walks priorities and returns the first source that matches the
// entry and satisfies provides. Shared by the field-group cascade and
// ResolveGear, which decides "provides" by a lookup instead of the payload.
func pickWinnerBy(sources []ActivitySource, priorities []string, provides func(*ActivitySource) bool) (ActivitySource, bool) {
	if len(priorities) == 0 {
		return ActivitySource{}, false
	}

	// Build the explicit-provider set so AnyProvider can exclude them.
	named := make(map[string]struct{}, len(priorities))
	for _, p := range priorities {
		if p != AnyProvider {
			named[p] = struct{}{}
		}
	}

	for _, p := range priorities {
		if s, ok := pickWinnerForPriority(sources, p, named, provides); ok {
			return s, true
		}
	}

	return ActivitySource{}, false
}

// poolPriority is the priority entry naming s's worker pool,
// "<provider>.<worker_name>" (docs/adr/0006); a bare provider entry matches
// every pool of that provider.
func poolPriority(s *ActivitySource) string {
	return s.Provider + "." + s.SourceWorkerName
}

// pickWinnerForPriority returns the first source matching priority entry p that satisfies provides.
func pickWinnerForPriority(sources []ActivitySource, p string, named map[string]struct{}, provides func(*ActivitySource) bool) (ActivitySource, bool) {
	for i := range sources {
		s := &sources[i]
		if p == AnyProvider {
			_, byProvider := named[s.Provider]
			_, byPool := named[poolPriority(s)]
			if byProvider || byPool {
				continue
			}
		} else if p != s.Provider && p != poolPriority(s) {
			continue
		}
		if provides(s) {
			return *s, true
		}
	}
	return ActivitySource{}, false
}

// mergeApplyGroups applies each non-classification group's winner (pin, else cascade) to out.
func mergeApplyGroups(out *Activity, ordered []ActivitySource, policy MergePolicy, overrides map[FieldGroup]SourceID) {
	for _, g := range AllFieldGroups {
		if g == FieldGroupClassification || g == FieldGroupGear {
			continue // classification handled above; gear by ResolveGear
		}

		// A manual per-field source pin (override) wins over the cascade.
		winner, ok := overrideWinner(ordered, g, overrides)
		if !ok {
			winner, ok = pickWinner(ordered, g, policy)
		}
		if !ok {
			continue
		}

		switch g {
		case FieldGroupGPSTrack:
			// The stream-bearing source becomes the activity's primary
			// stream. No summary fields are written for this group.
			winnerID := winner.ID
			out.PrimaryStreamSourceID = &winnerID
			out.MergeProvenance.Set(g, winner.ID)

		case FieldGroupLaps:
			// Laps live in a separate table; we just record provenance.
			// The persistence layer hydrates the winning source's laps
			// when callers request them.
			out.MergeProvenance.Set(g, winner.ID)

		default:
			winner.Parsed.ApplyTo(g, out)
			out.MergeProvenance.Set(g, winner.ID)
		}
	}
}

// mergeDeriveInvariants fills fields derivable from the merged ones.
func mergeDeriveInvariants(out *Activity) {
	// If EndTime wasn't supplied by any time-group winner, derive it from
	// StartTime + ElapsedDuration. Source data is usually consistent but
	// some workers emit only one of the two fields.
	if out.EndTime.IsZero() && !out.StartTime.IsZero() && out.ElapsedDuration > 0 {
		out.EndTime = out.StartTime.Add(out.ElapsedDuration)
	}

	// Derived: recompute avg speed from merged distance ÷ moving-time so it
	// stays consistent when distance/duration win from different sources.
	if out.Summary.DistanceM != nil && *out.Summary.DistanceM > 0 && out.MovingDuration > 0 {
		v := *out.Summary.DistanceM / out.MovingDuration.Seconds()
		out.Summary.AvgSpeedMps = &v
	}
}
