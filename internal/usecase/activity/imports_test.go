package activity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	gearuc "github.com/opencairn-org/cairn-core/internal/usecase/gear"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// fakeIngester records calls; IngestManual seeds a one-source activity.
type fakeIngester struct {
	repo        *inmem.ActivityRepo
	files       int
	manuals     int
	validateErr error
}

func (f *fakeIngester) IngestFile(context.Context, port.UploadInput) (port.UploadResult, error) {
	f.files++
	return port.UploadResult{Action: "imported_new"}, nil
}

func (f *fakeIngester) ValidateManual(port.ManualActivity) error { return f.validateErr }

func (f *fakeIngester) IngestManual(_ context.Context, in port.ManualActivity) (port.UploadResult, error) {
	f.manuals++
	aid, sid := domain.ActivityID(uuid.New()), domain.SourceID(uuid.New())
	f.repo.Activities[aid] = domain.Activity{ID: aid, UserID: in.UserID, Type: domain.ActivityTypeRide, StartTime: saNow}
	f.repo.Sources[sid] = domain.ActivitySource{
		ID: sid, ActivityID: aid, UserID: in.UserID, Provider: "manual", ExternalID: uuid.NewString(),
		Status: domain.SourceStatusActive, ImportedAt: saNow,
		Parsed: domain.ActivitySourcePayload{Type: domain.ActivityTypeRide, StartTime: saNow, EndTime: saNow.Add(time.Hour), ElapsedDuration: time.Hour, Timezone: "UTC"},
	}
	return port.UploadResult{ActivityID: aid, SourceID: sid, Action: "imported_new"}, nil
}

type importsFixture struct {
	uc   *Imports
	ing  *fakeIngester
	repo *inmem.ActivityRepo
	gear *inmem.GearRepo
	user domain.UserID
}

func newImportsFixture(maxActivities int) *importsFixture {
	repo := inmem.NewActivityRepo()
	f := &importsFixture{repo: repo, ing: &fakeIngester{repo: repo}, gear: inmem.NewGearRepo(), user: domain.UserID(uuid.New())}
	f.uc = &Imports{
		Quota: view.NewQuota(repo, nil, maxActivities), Ingest: f.ing,
		Gear: gearuc.NewService(f.gear, nil, nil, nil, nil, nil, nil), Activities: repo,
		Recompute: NewRecomputeActivityFromSources(repo, inmem.DefaultSettings{}, nil, nil, nil, nil, inmem.PassTx{}, func() time.Time { return saNow }),
	}
	return f
}

func TestImports_QuotaGatesUploadAndManual(t *testing.T) {
	f := newImportsFixture(1)
	ctx := context.Background()
	if _, err := f.uc.IngestUpload(ctx, port.UploadInput{UserID: f.user, Raw: []byte("x")}); err != nil {
		t.Fatalf("under quota: %v", err)
	}
	aid := domain.ActivityID(uuid.New())
	f.repo.Activities[aid] = domain.Activity{ID: aid, UserID: f.user}

	_, err := f.uc.IngestUpload(ctx, port.UploadInput{UserID: f.user, Raw: []byte("x")})
	var qe *QuotaReachedError
	if !errors.As(err, &qe) || qe.Used != 1 || qe.Limit != 1 {
		t.Fatalf("over quota: %v", err)
	}
	var ae *port.ActionError
	if !errors.As(err, &ae) || ae.Kind != port.ActionErrorQuota {
		t.Errorf("quota error does not unwrap to an ActionErrorQuota: %v", err)
	}
	if _, err := f.uc.Manual(ctx, port.ManualActivity{UserID: f.user}, nil); !errors.As(err, &qe) {
		t.Errorf("manual over quota: %v", err)
	}
	if f.ing.files != 1 || f.ing.manuals != 0 {
		t.Errorf("ingested past the gate: files=%d manuals=%d", f.ing.files, f.ing.manuals)
	}
}

func TestImports_UploadRejectsEmptyAndOversized(t *testing.T) {
	f := newImportsFixture(0)
	for _, raw := range [][]byte{nil, make([]byte, port.MaxUploadBytes+1)} {
		_, err := f.uc.IngestUpload(context.Background(), port.UploadInput{UserID: f.user, Raw: raw})
		var ae *port.ActionError
		if !errors.As(err, &ae) || ae.Kind != port.ActionErrorInvalid {
			t.Errorf("len %d: %v", len(raw), err)
		}
	}
	if f.ing.files != 0 {
		t.Error("ingested a rejected file")
	}
}

func TestImports_ManualValidatesBeforeGear(t *testing.T) {
	f := newImportsFixture(0)
	f.ing.validateErr = port.NewActionError(port.ActionErrorInvalid, "bad type")
	missing := domain.GearID(uuid.New())
	_, err := f.uc.Manual(context.Background(), port.ManualActivity{UserID: f.user}, &missing)
	if err == nil || err.Error() != "bad type" {
		t.Errorf("got %v, want the validation error first", err)
	}
	f.ing.validateErr = nil
	_, err = f.uc.Manual(context.Background(), port.ManualActivity{UserID: f.user}, &missing)
	var ae *port.ActionError
	if !errors.As(err, &ae) || ae.Msg != "gear not found" {
		t.Errorf("unknown gear: %v", err)
	}
	if f.ing.manuals != 0 {
		t.Error("ingested an invalid entry")
	}
}

func TestImports_ManualAttachesChosenGear(t *testing.T) {
	f := newImportsFixture(0)
	bike := domain.GearID(uuid.New())
	if err := f.gear.Save(context.Background(), domain.Gear{ID: bike, UserID: f.user, Kind: domain.GearKindBike, Name: "Bike"}); err != nil {
		t.Fatal(err)
	}
	out, err := f.uc.Manual(context.Background(), port.ManualActivity{UserID: f.user}, &bike)
	if err != nil {
		t.Fatal(err)
	}
	a := f.repo.Activities[out.ActivityID]
	if a.GearID == nil || *a.GearID != bike || a.GearSetBy != domain.GearSetByUser {
		t.Errorf("gear %v set by %q", a.GearID, a.GearSetBy)
	}
}

func TestImports_NoGearServiceRejectsGearID(t *testing.T) {
	f := newImportsFixture(0)
	f.uc.Gear = nil
	id := domain.GearID(uuid.New())
	_, err := f.uc.Manual(context.Background(), port.ManualActivity{UserID: f.user}, &id)
	var ae *port.ActionError
	if !errors.As(err, &ae) || ae.Msg != "invalid gear_id" {
		t.Errorf("got %v", err)
	}
}
