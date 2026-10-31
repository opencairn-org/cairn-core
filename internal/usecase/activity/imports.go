package activity

import (
	"context"
	"log/slog"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	gearuc "github.com/opencairn-org/cairn-core/internal/usecase/gear"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// QuotaReachedError refuses an import of a user at their activity quota. It
// unwraps to an ActionErrorQuota.
type QuotaReachedError struct{ Used, Limit int }

func (e *QuotaReachedError) Error() string { return "activity quota reached" }

func (e *QuotaReachedError) Unwrap() error {
	return port.NewActionError(port.ActionErrorQuota, e.Error())
}

// importActivityStore reads and saves merged activities.
type importActivityStore interface {
	port.ActivityReader
	port.ActivityWriter
}

// Imports are the user-initiated imports (file upload, manual entry): the
// quota gate, then the shared ingest path.
type Imports struct {
	Quota  *view.Quota // nil → no quota
	Ingest port.ImportIngester
	// Gear validates and attaches a manual entry's gear choice (nil → any
	// gear_id is invalid); Activities and Recompute store it.
	Gear       *gearuc.Service
	Activities importActivityStore
	Recompute  *RecomputeActivityFromSources
	Log        *slog.Logger
}

var _ port.UploadIngester = (*Imports)(nil)

func (i *Imports) log() *slog.Logger {
	if i.Log == nil {
		return slog.Default()
	}
	return i.Log
}

// Gate refuses a user at their quota. A counting failure reads as no usage so
// a transient hiccup never wrongly blocks an import.
func (i *Imports) Gate(ctx context.Context, userID domain.UserID) error {
	if i.Quota == nil {
		return nil
	}
	if st, _ := i.Quota.Status(ctx, userID); st.OverLimit {
		return &QuotaReachedError{Used: st.Used, Limit: st.Limit}
	}
	return nil
}

// IngestUpload gates, checks the file size and ingests the file.
func (i *Imports) IngestUpload(ctx context.Context, in port.UploadInput) (port.UploadResult, error) {
	if err := i.Gate(ctx, in.UserID); err != nil {
		return port.UploadResult{}, err
	}
	if len(in.Raw) == 0 {
		return port.UploadResult{}, port.NewActionError(port.ActionErrorInvalid, "empty file")
	}
	if len(in.Raw) > port.MaxUploadBytes {
		return port.UploadResult{}, port.NewActionError(port.ActionErrorInvalid, "file exceeds the upload limit")
	}
	return i.Ingest.IngestFile(ctx, in)
}

// Manual gates, validates and ingests a manual entry, then attaches the
// chosen gear. Ingest errors are returned unwrapped.
func (i *Imports) Manual(ctx context.Context, in port.ManualActivity, gearID *domain.GearID) (port.UploadResult, error) {
	if err := i.Gate(ctx, in.UserID); err != nil {
		return port.UploadResult{}, err
	}
	if err := i.Ingest.ValidateManual(in); err != nil {
		return port.UploadResult{}, err
	}
	if gearID != nil {
		if i.Gear == nil {
			return port.UploadResult{}, port.NewActionError(port.ActionErrorInvalid, "invalid gear_id")
		}
		if _, err := i.Gear.Get(ctx, in.UserID, *gearID); err != nil {
			return port.UploadResult{}, port.NewActionError(port.ActionErrorInvalid, "gear not found")
		}
	}
	out, err := i.Ingest.IngestManual(ctx, in)
	if err != nil {
		i.log().Error("manual ingest failed", "user_id", in.UserID, "error", err)
		return port.UploadResult{}, err
	}
	if gearID != nil {
		if err := i.setGear(ctx, out.ActivityID, *gearID); err != nil {
			// The activity exists; only the gear choice is lost.
			i.log().Warn("manual gear attach failed", "activity_id", out.ActivityID, "error", err)
		}
	}
	return out, nil
}

// setGear stores the user's explicit gear choice and re-merges, which also
// refreshes totals.
func (i *Imports) setGear(ctx context.Context, id domain.ActivityID, gearID domain.GearID) error {
	a, err := i.Activities.GetActivity(ctx, id)
	if err != nil {
		return err
	}
	a.GearID, a.GearSetBy = &gearID, domain.GearSetByUser
	if err := i.Activities.SaveActivity(ctx, a); err != nil {
		return err
	}
	_, err = i.Recompute.Execute(ctx, id)
	return err
}
