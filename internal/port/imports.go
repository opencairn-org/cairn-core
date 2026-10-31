package port

import (
	"context"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// ManualActivity is a manually entered activity: summary values, no stream.
// Type, StartTime (RFC 3339) and ElapsedSeconds are required.
type ManualActivity struct {
	UserID         domain.UserID
	Type           string
	Discipline     string
	Title          string
	Description    string
	StartTime      string
	Timezone       string
	ElapsedSeconds float64
	MovingSeconds  float64
	IsVirtual      bool
	IsEbike        bool
	IsCommute      bool
	IsRace         bool
	CustomSubtype  string

	DistanceM       *float64
	ElevationGainM  *float64
	AvgHeartRateBpm *int32
	AvgPowerW       *int32
	CaloriesKcal    *int32
}

// ImportIngester runs a user import (file upload, manual entry) through the
// worker ingest path. Implemented in package main over the worker result
// envelope; quota and gear are the caller's business.
type ImportIngester interface {
	// IngestFile parses the file and ingests it; an unparseable file is an
	// ActionErrorInvalid.
	IngestFile(ctx context.Context, in UploadInput) (UploadResult, error)
	// ValidateManual reports a malformed entry (ActionErrorInvalid) without writing.
	ValidateManual(in ManualActivity) error
	// IngestManual ingests a validated entry as a fresh "manual" source.
	IngestManual(ctx context.Context, in ManualActivity) (UploadResult, error)
}
