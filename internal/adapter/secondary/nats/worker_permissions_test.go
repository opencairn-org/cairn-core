package nats

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

func TestWorkerTemplateNamesMatchBootstrap(t *testing.T) {
	for got, want := range map[string]string{
		domain.WorkerJobStream:      StreamJobs,
		domain.WorkerWebhookStream:  StreamWebhooks,
		domain.WorkerPresenceBucket: KVWorkerPresence,
		domain.WorkerManifestBucket: KVWorkerManifests,
		domain.WorkerRateBucket:     KVRateLimits,
		domain.CoreInfoBucket:       KVCoreInfo,
	} {
		if got != want {
			t.Errorf("template name %q != bootstrap name %q", got, want)
		}
	}
}

// newPermissionedWorker bootstraps a server as admin and connects a worker
// restricted to the standard template, as the auth-callout would issue it.
func newPermissionedWorker(t *testing.T) (admin, worker *Bus) {
	t.Helper()
	tpl := domain.StandardPermissionTemplate("strava", "w1")
	s := startServer(t, &server.Options{Users: []*server.User{
		{Username: "admin", Password: "admin"},
		{Username: "worker", Password: "worker", Permissions: &server.Permissions{
			Publish:   &server.SubjectPermission{Allow: tpl.AllowPublish},
			Subscribe: &server.SubjectPermission{Allow: tpl.AllowSubscribe},
		}},
	}})
	connect := func(user string) *Bus {
		nc, err := nats.Connect(s.ClientURL(), nats.UserInfo(user, user), nats.ErrorHandler(func(*nats.Conn, *nats.Subscription, error) {}))
		if err != nil {
			t.Fatal(err)
		}
		b, err := NewBusFromConn(nc, user, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(b.Close)
		return b
	}
	admin = connect("admin")
	if err := admin.BootstrapStreams(context.Background()); err != nil {
		t.Fatal(err)
	}
	return admin, connect("worker")
}

func TestWorkerTemplateAllowsWorkerJetStreamUse(t *testing.T) {
	admin, worker := newPermissionedWorker(t)
	assertWorkerAllowed(t, admin, worker)
}

// assertWorkerAllowed runs what a w1-strava worker does under the standard template.
func assertWorkerAllowed(t *testing.T, admin, worker *Bus) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, cc := range []port.ConsumerConfig{
		{Stream: StreamJobs, Durable: "w1__fetch", Subject: "cairn.jobs.fetch_source.strava.w1"},
		{Stream: StreamJobs, Durable: "w1__gear", Subject: "cairn.jobs.import_gear.strava.w1"},
		{Stream: StreamJobs, Durable: "strava__fetch", Subject: "cairn.jobs.fetch_source.strava"}, // legacy drain
		{Stream: StreamJobs, Durable: "strava__reconcile", Subject: "cairn.jobs.reconcile.strava"},
		{Stream: StreamJobs, Durable: "strava__metrics", Subject: "cairn.jobs.import_metrics.strava"},
		{Stream: StreamWebhooks, Durable: "strava__webhook_event", Subject: "cairn.webhooks.w1-strava.event"},
	} {
		ps, err := worker.Pull(ctx, cc)
		if err != nil {
			t.Fatalf("pull %s: %v", cc.Subject, err)
		}
		if err := admin.Publish(ctx, cc.Subject, "", []byte("job")); err != nil {
			t.Fatal(err)
		}
		msgs, err := ps.Fetch(ctx, 1)
		if err != nil || len(msgs) != 1 {
			t.Fatalf("fetch %s: %v (%d msgs)", cc.Subject, err, len(msgs))
		}
		if err := msgs[0].InProgress(ctx); err != nil {
			t.Fatalf("in progress: %v", err)
		}
		if err := msgs[0].Ack(ctx); err != nil {
			t.Fatalf("ack: %v", err)
		}
	}

	for _, subj := range []string{
		"cairn.results.fetch_source.strava",
		"cairn.results.import_metrics.strava",
		"cairn.results.reconcile.strava.acct-1",
		"cairn.jobs.fetch_source.strava.w1", // reconcile sub-job to its own pool
	} {
		if err := worker.Publish(ctx, subj, "", []byte("result")); err != nil {
			t.Fatalf("publish %s: %v", subj, err)
		}
	}

	// Another pool's jobs are out of reach (docs/adr/0006).
	denyCtx, denyCancel := context.WithTimeout(ctx, 2*time.Second)
	defer denyCancel()
	if _, err := worker.Pull(denyCtx, port.ConsumerConfig{Stream: StreamJobs, Durable: "w2__fetch", Subject: "cairn.jobs.fetch_source.strava.w2"}); err == nil {
		t.Fatal("pulled another pool's job subject")
	}

	// Core request/reply: the worker asks for credentials and serves discover.
	if _, err := admin.RespondTo(ctx, "cairn.creds.strava.get", func(context.Context, []byte) ([]byte, error) {
		return []byte("creds"), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := admin.Conn().Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Request(ctx, "cairn.creds.strava.get", []byte("{}"), 2*time.Second); err != nil {
		t.Fatalf("creds request: %v", err)
	}
	if _, err := worker.RespondTo(ctx, "cairn.discover.strava.w1", func(context.Context, []byte) ([]byte, error) {
		return []byte("items"), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := worker.Conn().Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Request(ctx, "cairn.discover.strava.w1", []byte("{}"), 2*time.Second); err != nil {
		t.Fatalf("discover reply: %v", err)
	}
	if err := worker.Publish(ctx, "cairn.workers.w1-strava.heartbeat", "", []byte("hb")); err != nil {
		t.Fatalf("core publish: %v", err)
	}

	// Core info: readable, not writable.
	if akv, err := admin.KV(KVCoreInfo); err != nil {
		t.Fatal(err)
	} else if _, err := akv.Put(ctx, "info", []byte(`{"release":"v0.5.0"}`)); err != nil {
		t.Fatal(err)
	}
	ikv, err := worker.KV(KVCoreInfo)
	if err != nil {
		t.Fatalf("kv %s: %v", KVCoreInfo, err)
	}
	if e, err := ikv.Get(ctx, "info"); err != nil || len(e.Value) == 0 {
		t.Fatalf("read core info: %v", err)
	}
	wctx, wcancel := context.WithTimeout(ctx, 2*time.Second)
	defer wcancel()
	if _, err := ikv.Put(wctx, "info", []byte("{}")); err == nil {
		t.Fatal("worker overwrote core info")
	}

	for _, bucket := range []string{KVWorkerPresence, KVWorkerManifests, KVRateLimits} {
		kv, err := worker.KV(bucket)
		if err != nil {
			t.Fatalf("kv %s: %v", bucket, err)
		}
		if _, err := kv.Get(ctx, "strava"); !errors.Is(err, port.ErrKVKeyNotFound) {
			t.Fatalf("kv %s get: %v", bucket, err)
		}
		rev, err := kv.Put(ctx, "strava", []byte("v1"))
		if err != nil {
			t.Fatalf("kv %s put: %v", bucket, err)
		}
		if _, ok, err := kv.CompareAndSet(ctx, "strava", []byte("v2"), rev); err != nil || !ok {
			t.Fatalf("kv %s cas: %v %v", bucket, ok, err)
		}
	}
}

func TestWorkerTemplateDeniesForeignJetStreamUse(t *testing.T) {
	_, worker := newPermissionedWorker(t)
	assertWorkerDenied(t, worker)
}

// assertWorkerDenied checks a w1-strava worker cannot reach beyond its template.
func assertWorkerDenied(t *testing.T, worker *Bus) {
	t.Helper()
	// Denied requests surface as timeouts; keep each one short.
	short := func() context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		t.Cleanup(cancel)
		return ctx
	}

	if _, err := worker.Pull(short(), port.ConsumerConfig{
		Stream: StreamJobs, Durable: "steal", Subject: "cairn.jobs.fetch_source.garmin",
	}); err == nil {
		t.Error("consumer on another provider's jobs must be denied")
	}
	if err := worker.Publish(short(), "cairn.jobs.reconcile.strava", "", []byte("job")); err == nil {
		t.Error("enqueueing jobs other than fetch_source must be denied")
	}
	if _, err := worker.Request(short(), "cairn.creds.garmin.get", []byte("{}"), time.Second); err == nil {
		t.Error("another provider's credentials must be denied")
	}
	if _, err := worker.Pull(short(), port.ConsumerConfig{
		Stream: StreamResults, Durable: "steal", Subject: "cairn.results.>",
	}); err == nil {
		t.Error("consumer on the results stream must be denied")
	}
	if _, err := worker.JS().KeyValue(short(), KVBlobHandles); err == nil {
		t.Error("blob handle bucket must be denied")
	}
	if err := worker.JS().DeleteStream(short(), StreamJobs); err == nil {
		t.Error("stream delete must be denied")
	}
}
