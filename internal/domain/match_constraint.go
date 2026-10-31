package domain

import "time"

// MatchConstraint is a user's manual matching decision fed into clustering:
// must_link forces two sources together, cannot_link keeps them apart.
type MatchConstraint struct {
	ID        MatchConstraintID
	UserID    UserID
	SourceA   SourceID // canonical order: SourceA < SourceB
	SourceB   SourceID
	Kind      ConstraintKind
	Reason    string
	CreatedAt time.Time
}

// ConstraintKind enumerates the manual matching decisions.
type ConstraintKind string

const (
	ConstraintMustLink   ConstraintKind = "must_link"
	ConstraintCannotLink ConstraintKind = "cannot_link"
	// ConstraintSuspectedDuplicate is a matcher suggestion, not a decision: a
	// high-scoring same-pool pair awaiting the owner's merge or keep-both.
	ConstraintSuspectedDuplicate ConstraintKind = "suspected_duplicate"
)

// Valid reports whether k is a known constraint kind.
func (k ConstraintKind) Valid() bool {
	return k == ConstraintMustLink || k == ConstraintCannotLink || k == ConstraintSuspectedDuplicate
}
