package connect

import (
	"context"
	"testing"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
)

type fakeCommitter struct {
	repo *inmem.ActivityRepo
	got  []domain.SourcePreviewID
}

func (f *fakeCommitter) Commit(_ context.Context, _ domain.UserID, id domain.SourcePreviewID) (domain.Activity, error) {
	f.got = append(f.got, id)
	for _, a := range f.repo.Activities {
		return a, nil
	}
	return domain.Activity{}, domain.ErrNotFound
}

func TestSourcePreviewRPCs(t *testing.T) {
	f := newActFixture(t)
	sid, aid := seedActivity(f.repo, f.user, "garmin", actNow, 10000)
	previews := inmem.NewSourcePreviewRepo()
	previews.ActivityOf = func(domain.SourceID) domain.ActivityID { return aid }
	f.srv.Previews = &activity.SourcePreviews{
		Repo: previews, Actions: f.srv.SourceActions, Recompute: f.srv.Recompute, Reimporter: f.reimp, Activities: f.repo,
		Now: func() time.Time { return actNow },
	}
	committer := &fakeCommitter{repo: f.repo}
	f.srv.PreviewCommitter = committer

	if _, err := (&ActivityServer{}).PreviewSourceReimport(f.ctx, &v1.PreviewSourceReimportRequest{ActivitySourceId: sid.String()}); connectrpc.CodeOf(err) != connectrpc.CodeUnavailable {
		t.Fatalf("unwired: %v", err)
	}
	res, err := f.srv.PreviewSourceReimport(f.ctx, &v1.PreviewSourceReimportRequest{ActivitySourceId: sid.String(), PreferLocalReparse: true})
	if err != nil || res.GetJobId() != "reparse:"+sid.String() || res.GetPreviewId() == "" {
		t.Fatalf("start: %v %v", res, err)
	}
	got, err := f.srv.GetSourcePreview(f.ctx, &v1.GetSourcePreviewRequest{PreviewId: res.GetPreviewId()})
	if err != nil || got.GetPreview().GetStatus() != v1.SourcePreviewStatus_SOURCE_PREVIEW_STATUS_PENDING || got.GetPreview().GetActivityId() != aid.String() {
		t.Fatalf("get: %v %v", got, err)
	}
	if _, err := f.srv.GetSourcePreview(f.ctx, &v1.GetSourcePreviewRequest{PreviewId: "nope"}); connectrpc.CodeOf(err) != connectrpc.CodeInvalidArgument {
		t.Fatalf("bad id: %v", err)
	}
	if _, err := f.srv.GetSourcePreview(f.ctx, &v1.GetSourcePreviewRequest{PreviewId: uuid.NewString()}); connectrpc.CodeOf(err) != connectrpc.CodeNotFound {
		t.Fatalf("unknown: %v", err)
	}
	// The committer owns the ingest; the RPC renders the activity it returns.
	commit, err := f.srv.CommitSourcePreview(f.ctx, &v1.CommitSourcePreviewRequest{PreviewId: res.GetPreviewId()})
	if err != nil || commit.GetActivity().GetId() != aid.String() || len(committer.got) != 1 {
		t.Fatalf("commit: %v %v", commit, err)
	}
	// The fake committer leaves the row; a second start is refused until it is gone.
	if _, err := f.srv.PreviewSourceReimport(f.ctx, &v1.PreviewSourceReimportRequest{ActivitySourceId: sid.String()}); connectrpc.CodeOf(err) != connectrpc.CodeFailedPrecondition {
		t.Fatalf("pending twice: %v", err)
	}
	if _, err := f.srv.DiscardSourcePreview(f.ctx, &v1.DiscardSourcePreviewRequest{PreviewId: res.GetPreviewId()}); err != nil || len(previews.Rows) != 0 {
		t.Fatalf("discard: %v rows %d", err, len(previews.Rows))
	}
}
