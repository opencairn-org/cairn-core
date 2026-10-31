package pipeline

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/activity"
)

type published struct {
	subject, msgID string
	body           []byte
	ctxErr         error
}

type busSpy struct {
	err  error
	msgs []published
}

func (b *busSpy) Publish(ctx context.Context, subject, msgID string, body []byte, _ ...port.PublishOpt) error {
	b.msgs = append(b.msgs, published{subject, msgID, body, ctx.Err()})
	return b.err
}

// ctxStreams records the context each aggregate refresh ran under.
type ctxStreams struct {
	port.StreamRepo
	mu   sync.Mutex
	errs []error
}

func (s *ctxStreams) RefreshAggregates(ctx context.Context, _ domain.SourceID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs = append(s.errs, ctx.Err())
	return nil
}

func softDeletable(f *fixture) domain.ActivityID {
	aid, srcs := f.seed(1)
	s := f.repo.Sources[srcs[0]]
	s.Status = domain.SourceStatusDetached
	f.repo.Sources[srcs[0]] = s
	return aid
}

func TestRemergePublishesInsteadOfRunningInline(t *testing.T) {
	f := newFixture()
	bus := &busSpy{}
	f.p.Bus = bus
	aid := softDeletable(f)

	if _, err := f.p.Remerge(context.Background(), aid); err != nil {
		t.Fatal(err)
	}
	if len(f.fed.deleted) != 0 {
		t.Fatalf("follow-ups ran inline: %+v", f.fed.deleted)
	}
	if len(bus.msgs) != 1 || bus.msgs[0].subject != ActivityChangedSubject || bus.msgs[0].msgID == "" {
		t.Fatalf("published: %+v", bus.msgs)
	}
	ev, err := DecodeActivityChanged(bus.msgs[0].body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != ChangeRemerged || ev.ActivityID != aid || ev.UserID != f.user || !ev.SoftDeleted || ev.MsgID != bus.msgs[0].msgID {
		t.Fatalf("event: %+v", ev)
	}
	// The consumer side runs what the event owes.
	if err := f.p.RunFollowUps(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(f.fed.deleted) != 1 || f.fed.deleted[0] != aid {
		t.Fatalf("federation delete: %+v", f.fed.deleted)
	}
}

func TestPublishFailureFallsBackInline(t *testing.T) {
	f := newFixture()
	f.p.Bus = &busSpy{err: errors.New("nats down")}
	aid := softDeletable(f)
	if _, err := f.p.Remerge(context.Background(), aid); err != nil {
		t.Fatal(err)
	}
	if len(f.fed.deleted) != 1 {
		t.Fatalf("inline fallback did not run: %+v", f.fed.deleted)
	}
}

func TestCancelledRequestDoesNotCancelFollowUps(t *testing.T) {
	f := newFixture()
	streams := &ctxStreams{}
	f.p.Streams = streams
	aid, srcs := f.seed(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	f.p.dispatch(ctx, ActivityChanged{Kind: ChangeBorrowed, UserID: f.user, ActivityID: aid, SourceID: srcs[0]})
	if len(streams.errs) != 1 || streams.errs[0] != nil {
		t.Fatalf("inline follow-up context: %v", streams.errs)
	}

	bus := &busSpy{}
	f.p.Bus = bus
	f.p.dispatch(ctx, ActivityChanged{Kind: ChangeBorrowed, UserID: f.user, ActivityID: aid, SourceID: srcs[0]})
	if len(bus.msgs) != 1 || bus.msgs[0].ctxErr != nil {
		t.Fatalf("publish context: %+v", bus.msgs)
	}
}

func TestFollowUpsFederationGate(t *testing.T) {
	cases := []struct {
		name       string
		action     activity.IngestAction
		age        time.Duration
		redelivery bool
		want       int
	}{
		{"new and recent", activity.IngestActionImportedNew, time.Hour, false, 1},
		{"merged into existing", activity.IngestActionMergedIntoExisting, time.Hour, false, 0},
		{"historical", activity.IngestActionImportedNew, FederationMaxAge + time.Hour, false, 0},
		{"redelivery", activity.IngestActionImportedNew, time.Hour, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture()
			aid, srcs := f.seed(1)
			a := f.repo.Activities[aid]
			a.StartTime = tNow.Add(-c.age)
			f.repo.Activities[aid] = a
			err := f.p.RunFollowUps(context.Background(), ActivityChanged{
				Kind: ChangeIngested, UserID: f.user, ActivityID: aid, SourceID: srcs[0], Action: c.action, Redelivery: c.redelivery,
			})
			if err != nil || len(f.fed.created) != c.want {
				t.Fatalf("err=%v created=%v want %d", err, f.fed.created, c.want)
			}
		})
	}
}

func TestFollowUpsForVanishedActivityAck(t *testing.T) {
	f := newFixture()
	err := f.p.RunFollowUps(context.Background(), ActivityChanged{Kind: ChangeIngested, UserID: f.user, ActivityID: domain.ActivityID(uuid.New()), SourceID: domain.SourceID(uuid.New())})
	if err != nil {
		t.Fatal(err)
	}
}

func TestActivityChangedRoundTrip(t *testing.T) {
	in := ActivityChanged{
		Kind: ChangeIngested, UserID: domain.UserID(uuid.New()), ActivityID: domain.ActivityID(uuid.New()),
		SourceID: domain.SourceID(uuid.New()), Action: activity.IngestActionMergedIntoExisting, MsgID: "m-1",
	}
	b, err := in.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeActivityChanged(b)
	if err != nil || out != in {
		t.Fatalf("round trip: %v %+v", err, out)
	}
	for _, bad := range []string{`{`, `{"kind":"nope"}`, `{"kind":"ingested","user_id":"x"}`} {
		if _, err := DecodeActivityChanged([]byte(bad)); err == nil {
			t.Errorf("%s decoded", bad)
		}
	}
}
