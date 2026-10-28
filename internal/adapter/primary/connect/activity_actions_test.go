package connect

import (
	"context"
	"errors"
	"testing"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
	dmatch "github.com/opencairn-org/cairn-core/internal/domain/match"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
	matchuc "github.com/opencairn-org/cairn-core/internal/usecase/match"
)

var actNow = time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)

// --- fakes -----------------------------------------------------------------

type fakeBestEfforts struct {
	port.BestEffortRepo
	records []domain.PersonalRecord
}

func (f *fakeBestEfforts) ListPersonalRecords(context.Context, domain.UserID) ([]domain.PersonalRecord, error) {
	return f.records, nil
}

type fakeClassOverrides struct {
	port.ClassificationOverrideRepo
	set map[domain.ActivityID]domain.ClassificationOverride
}

func (f *fakeClassOverrides) Get(_ context.Context, id domain.ActivityID) (domain.ClassificationOverride, error) {
	return f.set[id], nil
}
func (f *fakeClassOverrides) Set(_ context.Context, o domain.ClassificationOverride) error {
	f.set[o.ActivityID] = o
	return nil
}

type fakeReimporter struct {
	calls []string
	err   error
}

func (f *fakeReimporter) Refetch(_ context.Context, _ domain.UserID, src domain.ActivitySource) (string, error) {
	f.calls = append(f.calls, "refetch:"+src.ID.String())
	return "refetch:" + src.ID.String(), f.err
}
func (f *fakeReimporter) Reparse(_ context.Context, _ domain.UserID, src domain.ActivitySource) (string, error) {
	f.calls = append(f.calls, "reparse:"+src.ID.String())
	return "reparse:" + src.ID.String(), f.err
}

type fakeBlobs struct {
	port.BlobStore
	objects map[string][]byte
	signed  []string
}

func (f *fakeBlobs) PresignUpload(_ context.Context, key string, opts port.PresignUploadOpts) (port.PresignedURL, error) {
	f.signed = append(f.signed, key)
	return port.PresignedURL{URL: "https://s3.test/" + key, Method: "PUT",
		RequiredHeaders: map[string]string{"Content-Type": opts.ContentType}, ExpiresAt: actNow.Add(opts.Expiry)}, nil
}
func (f *fakeBlobs) Get(_ context.Context, key string) ([]byte, string, error) {
	b, ok := f.objects[key]
	if !ok {
		return nil, "", errors.New("no such key")
	}
	return b, "application/octet-stream", nil
}

// fakeIngester seeds a singleton activity + source for the upload, as the
// real pipeline does, and records the input.
type fakeIngester struct {
	repo *inmem.ActivityRepo
	in   port.UploadInput
	err  error
}

func (f *fakeIngester) IngestUpload(_ context.Context, in port.UploadInput) (port.UploadResult, error) {
	f.in = in
	if f.err != nil {
		return port.UploadResult{}, f.err
	}
	sid, aid := seedActivity(f.repo, in.UserID, "upload", actNow, 5000)
	return port.UploadResult{ActivityID: aid, SourceID: sid, Action: "imported_new"}, nil
}

// --- fixture ---------------------------------------------------------------

type actFixture struct {
	srv   *ActivityServer
	repo  *inmem.ActivityRepo
	deny  *inmem.SourceDenylist
	reimp *fakeReimporter
	blobs *fakeBlobs
	ing   *fakeIngester
	class *fakeClassOverrides
	user  domain.UserID
	ctx   context.Context
}

func newActFixture(t *testing.T) *actFixture {
	t.Helper()
	f := &actFixture{repo: inmem.NewActivityRepo(), deny: &inmem.SourceDenylist{}, reimp: &fakeReimporter{},
		blobs: &fakeBlobs{objects: map[string][]byte{}}, class: &fakeClassOverrides{set: map[domain.ActivityID]domain.ClassificationOverride{}},
		user: domain.UserID(uuid.New())}
	f.ing = &fakeIngester{repo: f.repo}
	now := func() time.Time { return actNow }
	recompute := activity.NewRecomputeActivityFromSources(f.repo, inmem.DefaultSettings{}, nil, nil, f.class, nil, inmem.PassTx{}, now)
	recluster := matchuc.NewReclusterBucket(f.repo, recompute, f.deny, inmem.PassTx{}, nil, dmatch.DefaultOptions(), nil)
	f.srv = NewActivityServer(f.repo, nil, &fakeBestEfforts{}, recompute, f.class, nil, nil)
	f.srv.SourceActions = activity.NewSourceActions(f.repo, f.deny, recompute, nil, now)
	f.srv.SourceLinks = matchuc.NewSourceLinks(f.repo, recluster)
	f.srv.Reimporter = f.reimp
	f.srv.Blobs = f.blobs
	f.srv.Uploads = f.ing
	f.ctx = context.WithValue(t.Context(), ctxKeyUserID, f.user)
	return f
}

// seedActivity adds a singleton activity with one active source.
func seedActivity(r *inmem.ActivityRepo, user domain.UserID, provider string, start time.Time, distM float64) (domain.SourceID, domain.ActivityID) {
	sid := domain.SourceID(uuid.New())
	aid := domain.ActivityID(uuid.New())
	d := distM
	r.Sources[sid] = domain.ActivitySource{
		ID: sid, ActivityID: aid, UserID: user, Provider: provider, ExternalID: provider + "-" + sid.String(),
		Status: domain.SourceStatusActive, ImportedAt: start,
		Parsed: domain.ActivitySourcePayload{
			Type: domain.ActivityTypeRide, Discipline: domain.DisciplineRideRoad, StartTime: start, EndTime: start.Add(time.Hour),
			ElapsedDuration: time.Hour, MovingDuration: 55 * time.Minute, Timezone: "UTC",
			Summary: domain.ActivitySummary{DistanceM: &d},
		},
	}
	r.Activities[aid] = domain.Activity{ID: aid, UserID: user, Type: domain.ActivityTypeRide, StartTime: start, Privacy: domain.PrivacyPrivate}
	return sid, aid
}

func wantCode(t *testing.T, err error, code connectrpc.Code) {
	t.Helper()
	if connectrpc.CodeOf(err) != code {
		t.Fatalf("got %v, want code %v", err, code)
	}
}

// --- ListActivitySources ----------------------------------------------------

func TestListActivitySources(t *testing.T) {
	f := newActFixture(t)
	s1, aid := seedActivity(f.repo, f.user, "garmin", actNow, 10000)
	s2 := domain.SourceID(uuid.New())
	f.repo.Sources[s2] = domain.ActivitySource{ID: s2, ActivityID: aid, UserID: f.user, Provider: "strava", Status: domain.SourceStatusDetached, StatusReason: "user_detach"}

	res, err := f.srv.ListActivitySources(f.ctx, &v1.ListActivitySourcesRequest{ActivityId: aid.String()})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetSources()) != 2 {
		t.Fatalf("want both sources incl. detached, got %d", len(res.GetSources()))
	}
	byID := map[string]*v1.ActivitySource{}
	for _, s := range res.GetSources() {
		byID[s.GetId()] = s
	}
	if byID[s1.String()].GetStatus() != v1.ActivitySourceStatus_ACTIVITY_SOURCE_STATUS_ACTIVE || byID[s2.String()].GetStatus() != v1.ActivitySourceStatus_ACTIVITY_SOURCE_STATUS_DETACHED {
		t.Fatalf("statuses wrong: %v", res.GetSources())
	}

	_, err = f.srv.ListActivitySources(context.WithValue(t.Context(), ctxKeyUserID, domain.UserID(uuid.New())), &v1.ListActivitySourcesRequest{ActivityId: aid.String()})
	wantCode(t, err, connectrpc.CodeNotFound)
	_, err = f.srv.ListActivitySources(f.ctx, &v1.ListActivitySourcesRequest{ActivityId: "nope"})
	wantCode(t, err, connectrpc.CodeInvalidArgument)
	_, err = f.srv.ListActivitySources(t.Context(), &v1.ListActivitySourcesRequest{ActivityId: aid.String()})
	wantCode(t, err, connectrpc.CodeUnauthenticated)
}

// --- DeleteActivity ---------------------------------------------------------

func TestDeleteActivity(t *testing.T) {
	f := newActFixture(t)
	sid, aid := seedActivity(f.repo, f.user, "garmin", actNow, 10000)
	if _, err := f.srv.DeleteActivity(f.ctx, &v1.DeleteActivityRequest{Id: aid.String()}); err != nil {
		t.Fatal(err)
	}
	if f.repo.Activities[aid].DeletedAt == nil || f.repo.Sources[sid].Status != domain.SourceStatusDetached {
		t.Fatal("activity not soft-deleted with its source detached")
	}
	if len(f.deny.Entries) != 1 {
		t.Fatalf("denylist entries %d", len(f.deny.Entries))
	}
	_, err := f.srv.DeleteActivity(f.ctx, &v1.DeleteActivityRequest{Id: aid.String()})
	wantCode(t, err, connectrpc.CodeNotFound)

	f.srv.SourceActions = nil
	_, err = f.srv.DeleteActivity(f.ctx, &v1.DeleteActivityRequest{Id: aid.String()})
	wantCode(t, err, connectrpc.CodeUnavailable)
}

// --- DetachSource -----------------------------------------------------------

func TestDetachSource_RemovesSource(t *testing.T) {
	f := newActFixture(t)
	s1, aid := seedActivity(f.repo, f.user, "garmin", actNow, 10000)
	s2 := domain.SourceID(uuid.New())
	src := f.repo.Sources[s1]
	src.ID, src.Provider, src.ExternalID = s2, "strava", "strava-x"
	f.repo.Sources[s2] = src

	res, err := f.srv.DetachSource(f.ctx, &v1.DetachSourceRequest{ActivitySourceId: s2.String()})
	if err != nil {
		t.Fatal(err)
	}
	if res.NewActivity != nil {
		t.Fatal("plain detach must not return a new activity")
	}
	if f.repo.Sources[s2].Status != domain.SourceStatusDetached || f.repo.Activities[aid].DeletedAt != nil {
		t.Fatal("source should be detached and the activity alive")
	}
	// A detached source is gone from the wire.
	_, err = f.srv.DetachSource(f.ctx, &v1.DetachSourceRequest{ActivitySourceId: s2.String()})
	wantCode(t, err, connectrpc.CodeNotFound)
}

func TestDetachSource_KeepAsNewActivitySplits(t *testing.T) {
	f := newActFixture(t)
	s1, _ := seedActivity(f.repo, f.user, "garmin", actNow, 30050)
	s2, _ := seedActivity(f.repo, f.user, "strava", actNow.Add(2*time.Second), 30000)
	if err := f.srv.SourceLinks.LinkActivities(f.ctx, f.user, f.repo.Sources[s1].ActivityID, f.repo.Sources[s2].ActivityID); err != nil {
		t.Fatalf("seed merge: %v", err)
	}
	merged := f.repo.Sources[s1].ActivityID
	if merged != f.repo.Sources[s2].ActivityID {
		t.Fatal("precondition: merged")
	}
	// Drop the must-link so the split's cannot-link wins.
	f.repo.Constraints = nil

	res, err := f.srv.DetachSource(f.ctx, &v1.DetachSourceRequest{ActivitySourceId: s2.String(), KeepAsNewActivity: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.GetNewActivity() == nil || res.GetNewActivity().GetId() != f.repo.Sources[s2].ActivityID.String() {
		t.Fatalf("new activity %v, source lives in %s", res.GetNewActivity(), f.repo.Sources[s2].ActivityID)
	}
	if f.repo.Sources[s1].ActivityID == f.repo.Sources[s2].ActivityID {
		t.Fatal("not split")
	}
	if f.repo.Sources[s2].Status != domain.SourceStatusActive {
		t.Fatal("a split source stays active")
	}
}

func TestDetachSource_SingleSourceSplitIsPrecondition(t *testing.T) {
	f := newActFixture(t)
	sid, _ := seedActivity(f.repo, f.user, "garmin", actNow, 10000)
	_, err := f.srv.DetachSource(f.ctx, &v1.DetachSourceRequest{ActivitySourceId: sid.String(), KeepAsNewActivity: true})
	wantCode(t, err, connectrpc.CodeFailedPrecondition)
}

// --- LinkSourceToActivity ---------------------------------------------------

func TestLinkSourceToActivity(t *testing.T) {
	f := newActFixture(t)
	s1, a1 := seedActivity(f.repo, f.user, "garmin", actNow, 30000)
	s2, _ := seedActivity(f.repo, f.user, "strava", actNow.Add(3*time.Hour), 25000)

	_, err := f.srv.LinkSourceToActivity(f.ctx, &v1.LinkSourceToActivityRequest{ActivitySourceId: s1.String(), TargetActivityId: a1.String()})
	wantCode(t, err, connectrpc.CodeFailedPrecondition)

	res, err := f.srv.LinkSourceToActivity(f.ctx, &v1.LinkSourceToActivityRequest{ActivitySourceId: s2.String(), TargetActivityId: a1.String(), Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if f.repo.Sources[s1].ActivityID != f.repo.Sources[s2].ActivityID {
		t.Fatal("not joined")
	}
	if res.GetActivity().GetId() != f.repo.Sources[s2].ActivityID.String() || len(res.GetActivity().GetSources()) != 2 {
		t.Fatalf("response %v", res.GetActivity())
	}
	other := domain.ActivityID(uuid.New())
	_, err = f.srv.LinkSourceToActivity(f.ctx, &v1.LinkSourceToActivityRequest{ActivitySourceId: s2.String(), TargetActivityId: other.String()})
	wantCode(t, err, connectrpc.CodeNotFound)
}

// --- ReimportSource ---------------------------------------------------------

func TestReimportSource(t *testing.T) {
	f := newActFixture(t)
	sid, _ := seedActivity(f.repo, f.user, "garmin", actNow, 10000)

	res, err := f.srv.ReimportSource(f.ctx, &v1.ReimportSourceRequest{ActivitySourceId: sid.String()})
	if err != nil || res.GetJobId() != "refetch:"+sid.String() {
		t.Fatalf("refetch: %v %v", res, err)
	}
	res, err = f.srv.ReimportSource(f.ctx, &v1.ReimportSourceRequest{ActivitySourceId: sid.String(), PreferLocalReparse: true})
	if err != nil || res.GetJobId() != "reparse:"+sid.String() {
		t.Fatalf("reparse: %v %v", res, err)
	}
	if len(f.reimp.calls) != 2 {
		t.Fatalf("calls %v", f.reimp.calls)
	}

	for kind, code := range map[port.ActionErrorKind]connectrpc.Code{
		port.ActionErrorUnavailable:  connectrpc.CodeUnavailable,
		port.ActionErrorUpstream:     connectrpc.CodeUnavailable,
		port.ActionErrorPrecondition: connectrpc.CodeFailedPrecondition,
		port.ActionErrorConflict:     connectrpc.CodeFailedPrecondition,
		port.ActionErrorNotFound:     connectrpc.CodeNotFound,
		port.ActionErrorInvalid:      connectrpc.CodeInvalidArgument,
		port.ActionErrorQuota:        connectrpc.CodeResourceExhausted,
		port.ActionErrorInternal:     connectrpc.CodeInternal,
	} {
		f.reimp.err = port.NewActionError(kind, "nope")
		_, err := f.srv.ReimportSource(f.ctx, &v1.ReimportSourceRequest{ActivitySourceId: sid.String()})
		if connectrpc.CodeOf(err) != code {
			t.Errorf("kind %d: got %v, want %v", kind, err, code)
		}
	}
	f.srv.Reimporter = nil
	_, err = f.srv.ReimportSource(f.ctx, &v1.ReimportSourceRequest{ActivitySourceId: sid.String()})
	wantCode(t, err, connectrpc.CodeUnavailable)
}

// --- ListRecords ------------------------------------------------------------

func TestListRecords(t *testing.T) {
	f := newActFixture(t)
	aid := domain.ActivityID(uuid.New())
	f.srv.BestEfforts = &fakeBestEfforts{records: []domain.PersonalRecord{
		{ActivityType: domain.ActivityTypeRun, Metric: domain.BestEffortMetricPace, WindowKind: domain.BestEffortWindowDistance, WindowValue: 5000, AchievedValue: 240, ActivityID: aid, Timestamp: actNow},
		{ActivityType: domain.ActivityTypeRide, Metric: domain.BestEffortMetricPower, WindowKind: domain.BestEffortWindowDuration, WindowValue: 1200, AchievedValue: 280, ActivityID: aid, Timestamp: actNow},
	}}
	res, err := f.srv.ListRecords(f.ctx, &v1.ListRecordsRequest{})
	if err != nil || len(res.GetRecords()) != 2 {
		t.Fatalf("all: %v %v", res, err)
	}
	r := res.GetRecords()[0]
	if r.GetUserId() != f.user.String() || r.GetActivityId() != aid.String() || r.GetWindowValue() != 5000 || r.GetMetric() != v1.BestEffortMetric_BEST_EFFORT_METRIC_PACE {
		t.Fatalf("record %v", r)
	}
	res, err = f.srv.ListRecords(f.ctx, &v1.ListRecordsRequest{ActivityType: v1.ActivityType_ACTIVITY_TYPE_RIDE})
	if err != nil || len(res.GetRecords()) != 1 || res.GetRecords()[0].GetMetric() != v1.BestEffortMetric_BEST_EFFORT_METRIC_POWER {
		t.Fatalf("type filter: %v %v", res, err)
	}
	res, err = f.srv.ListRecords(f.ctx, &v1.ListRecordsRequest{Metrics: []v1.BestEffortMetric{v1.BestEffortMetric_BEST_EFFORT_METRIC_PACE}})
	if err != nil || len(res.GetRecords()) != 1 {
		t.Fatalf("metric filter: %v %v", res, err)
	}
	d := v1.Discipline_DISCIPLINE_RUN_TRAIL
	_, err = f.srv.ListRecords(f.ctx, &v1.ListRecordsRequest{Discipline: &d})
	wantCode(t, err, connectrpc.CodeInvalidArgument)
	_, err = f.srv.ListRecords(f.ctx, &v1.ListRecordsRequest{TimeRange: &v1.TimeRange{}})
	wantCode(t, err, connectrpc.CodeInvalidArgument)
}

// --- Presign + Upload -------------------------------------------------------

func TestPresignActivityUpload(t *testing.T) {
	f := newActFixture(t)
	_, err := f.srv.PresignActivityUpload(f.ctx, &v1.PresignActivityUploadRequest{Extension: "zip"})
	wantCode(t, err, connectrpc.CodeInvalidArgument)
	_, err = f.srv.PresignActivityUpload(f.ctx, &v1.PresignActivityUploadRequest{Extension: "fit", SizeHintBytes: port.MaxUploadBytes + 1})
	wantCode(t, err, connectrpc.CodeInvalidArgument)

	res, err := f.srv.PresignActivityUpload(f.ctx, &v1.PresignActivityUploadRequest{Extension: ".FIT", SizeHintBytes: 1234})
	if err != nil {
		t.Fatal(err)
	}
	prefix := "uploads/" + f.user.String() + "/"
	if len(res.GetBlobId()) <= len(prefix) || res.GetBlobId()[:len(prefix)] != prefix || res.GetBlobId()[len(res.GetBlobId())-4:] != ".fit" {
		t.Fatalf("blob id %q", res.GetBlobId())
	}
	if res.GetUploadUrl() != "https://s3.test/"+res.GetBlobId() || res.GetRequiredHeaders()["Content-Type"] != "application/vnd.ant.fit" || res.GetExpiresAt() == nil {
		t.Fatalf("response %v", res)
	}
	f.srv.Blobs = nil
	_, err = f.srv.PresignActivityUpload(f.ctx, &v1.PresignActivityUploadRequest{Extension: "fit"})
	wantCode(t, err, connectrpc.CodeUnavailable)
}

func TestUploadActivity(t *testing.T) {
	f := newActFixture(t)
	key := "uploads/" + f.user.String() + "/abc.gpx"
	f.blobs.objects[key] = []byte("<gpx/>")

	// Another user's key, a traversal and a missing object are all NotFound.
	for _, bad := range []string{"uploads/" + uuid.NewString() + "/x.gpx", "uploads/" + f.user.String() + "/../y.gpx", "uploads/" + f.user.String() + "/missing.gpx"} {
		_, err := f.srv.UploadActivity(f.ctx, &v1.UploadActivityRequest{BlobId: bad})
		wantCode(t, err, connectrpc.CodeNotFound)
	}

	typ := v1.ActivityType_ACTIVITY_TYPE_RUN
	res, err := f.srv.UploadActivity(f.ctx, &v1.UploadActivityRequest{BlobId: key, Tags: []string{"race", " ", "race"}, TypeHint: &typ})
	if err != nil {
		t.Fatal(err)
	}
	if f.ing.in.RawBlobKey != key || string(f.ing.in.Raw) != "<gpx/>" || f.ing.in.Filename != "abc.gpx" || f.ing.in.UserID != f.user {
		t.Fatalf("ingester input %+v", f.ing.in)
	}
	aid := domain.ActivityID(uuid.MustParse(res.GetActivityId()))
	act := f.repo.Activities[aid]
	if len(act.Tags) != 1 || act.Tags[0] != "race" {
		t.Fatalf("tags %v", act.Tags)
	}
	if co := f.class.set[aid]; co.Type == nil || *co.Type != domain.ActivityTypeRun || act.Type != domain.ActivityTypeRun {
		t.Fatalf("type hint not applied: override=%+v activity=%s", co, act.Type)
	}
	if res.GetJobId() != "" {
		t.Fatal("synchronous ingest reports no job id")
	}

	f.ing.err = port.NewActionError(port.ActionErrorQuota, "quota")
	_, err = f.srv.UploadActivity(f.ctx, &v1.UploadActivityRequest{BlobId: key})
	wantCode(t, err, connectrpc.CodeResourceExhausted)
}

// --- scope classification of the new procedures -----------------------------

func TestNewActivityProceduresScope(t *testing.T) {
	for proc, mutating := range map[string]bool{
		"/cairn.v1.ActivityService/ListActivitySources":   false,
		"/cairn.v1.ActivityService/ListRecords":           false,
		"/cairn.v1.ActivityService/DeleteActivity":        true,
		"/cairn.v1.ActivityService/DetachSource":          true,
		"/cairn.v1.ActivityService/LinkSourceToActivity":  true,
		"/cairn.v1.ActivityService/ReimportSource":        true,
		"/cairn.v1.ActivityService/PresignActivityUpload": true,
		"/cairn.v1.ActivityService/UploadActivity":        true,
	} {
		if got := isMutatingProcedure(proc); got != mutating {
			t.Errorf("%s: mutating=%v, want %v", proc, got, mutating)
		}
	}
}
