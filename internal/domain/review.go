package domain

import "time"

// ReviewKind says why an activity is in the review queue.
type ReviewKind string

const (
	// ReviewKindMerge: a medium-confidence merge awaiting confirmation.
	ReviewKindMerge ReviewKind = "merge"
	// ReviewKindDuplicate: a likely same-provider re-import kept as a separate activity.
	ReviewKindDuplicate ReviewKind = "duplicate"
)

// ActivityReviewItem is the lightweight projection of a review-queue entry: an
// activity flagged needs_review (medium-confidence merge) or a possible
// duplicate of another activity (DuplicateOf).
type ActivityReviewItem struct {
	ID              ActivityID
	Title           string
	Type            ActivityType
	StartTime       time.Time
	MatchConfidence string
	Kind            ReviewKind
	DuplicateOf     ActivityID // zero unless Kind is ReviewKindDuplicate
}
