package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// ActionErrorKind classifies why a user action on an activity failed, so each
// primary adapter maps the same failure to its own status (HTTP vs Connect code).
type ActionErrorKind int

const (
	ActionErrorInternal     ActionErrorKind = iota
	ActionErrorInvalid                      // bad input
	ActionErrorNotFound                     // not the caller's, or gone
	ActionErrorPrecondition                 // the record lacks what the action needs (e.g. no archived blob)
	ActionErrorConflict                     // no compatible worker / pool state refuses it
	ActionErrorUnavailable                  // a required subsystem is not configured
	ActionErrorUpstream                     // object store or bus refused the request
	ActionErrorQuota                        // the user's activity quota is reached
	ActionErrorForbidden                    // visible but not permitted to this caller (private club, closed profile)
)

// ActionError carries a kind plus a user-facing message.
type ActionError struct {
	Kind ActionErrorKind
	Msg  string
}

func (e *ActionError) Error() string { return e.Msg }

// NewActionError builds an ActionError.
func NewActionError(kind ActionErrorKind, msg string) error {
	return &ActionError{Kind: kind, Msg: msg}
}

// SourceReimporter queues a re-import of one activity source. Implemented in
// package main over the NATS job bus and worker presence; injected into the
// primary adapters so REST and Connect share one dispatch path. Both return
// the queued job id. Failures are ActionErrors.
type SourceReimporter interface {
	// Refetch asks the account's worker pool to fetch the source fresh from
	// the provider (costs provider API budget).
	Refetch(ctx context.Context, userID domain.UserID, src domain.ActivitySource) (jobID string, err error)
	// Reparse asks a compatible worker to re-run its parser over the archived
	// raw blob (no provider budget).
	Reparse(ctx context.Context, userID domain.UserID, src domain.ActivitySource) (jobID string, err error)
}

// MaxUploadBytes caps a user-uploaded activity file (FIT/GPX/TCX).
const MaxUploadBytes = 64 << 20

// UploadInput is one user-uploaded activity file.
type UploadInput struct {
	UserID   domain.UserID
	Filename string
	Raw      []byte
	// RawBlobKey names the object the bytes already live under (presigned
	// upload); empty means the ingester archives Raw itself.
	RawBlobKey string
}

// UploadResult reports what the ingest did with the file.
type UploadResult struct {
	ActivityID domain.ActivityID
	SourceID   domain.SourceID
	// Action is the ingest action name ("imported_new", "reimported_existing", …).
	Action string
}

// UploadIngester parses an uploaded file and runs it through the ingest
// pipeline with its follow-ups. Implemented in package main; injected into the
// primary adapters so REST and Connect uploads behave identically. Failures
// are ActionErrors (quota, unparseable file) or internal errors.
type UploadIngester interface {
	IngestUpload(ctx context.Context, in UploadInput) (UploadResult, error)
}

// SourcePreviewCommitter ingests a ready preview's held worker result through
// the normal reimport path. Implemented in package main (it needs the
// result router); injected into the primary adapters.
type SourcePreviewCommitter interface {
	Commit(ctx context.Context, userID domain.UserID, previewID domain.SourcePreviewID) (domain.Activity, error)
}
