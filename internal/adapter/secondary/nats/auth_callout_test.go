package nats

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/opencairn-org/cairn-core/internal/auth"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/enrollment"
)

// fakeEnrollments is an in-memory port.WorkerEnrollmentRepo for the auth-callout tests.
type fakeEnrollments struct {
	port.WorkerEnrollmentRepo
	mu     sync.Mutex
	rows   []domain.WorkerEnrollment
	grants []domain.WorkerCredentialGrant
}

func (f *fakeEnrollments) CreateEnrollment(_ context.Context, e domain.WorkerEnrollment) (domain.WorkerEnrollment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, e)
	return e, nil
}

func (f *fakeEnrollments) ListEnrollments(context.Context, port.ListEnrollmentsFilter) ([]domain.WorkerEnrollment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.rows), nil
}

func (f *fakeEnrollments) FindEnrollmentByTokenHash(_ context.Context, hash []byte) (domain.WorkerEnrollment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.rows {
		if string(e.TokenHash) == string(hash) {
			return e, nil
		}
	}
	return domain.WorkerEnrollment{}, domain.ErrEnrollmentNotFound
}

func (f *fakeEnrollments) IncrementUses(_ context.Context, id domain.WorkerEnrollmentID) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].ID == id {
			f.rows[i].Uses++
			return f.rows[i].Uses, nil
		}
	}
	return 0, domain.ErrEnrollmentNotFound
}

func (f *fakeEnrollments) CreateGrant(_ context.Context, g domain.WorkerCredentialGrant) (domain.WorkerCredentialGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grants = append(f.grants, g)
	return g, nil
}

func (f *fakeEnrollments) grantCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.grants)
}

type fakeSigningKeys struct{ key domain.SigningKey }

func (f *fakeSigningKeys) GetActive(context.Context, string) (domain.SigningKey, error) {
	if f.key.PublicKey == "" {
		return domain.SigningKey{}, domain.ErrNotFound
	}
	return f.key, nil
}

func (f *fakeSigningKeys) CreateKey(_ context.Context, k domain.SigningKey) (domain.SigningKey, error) {
	f.key = k
	return k, nil
}

func (f *fakeSigningKeys) DeactivateAll(context.Context, string, time.Time) error { return nil }

type noTx struct{}

func (noTx) InTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }

// capturingIssuer records every user JWT the auth-callout mints.
type capturingIssuer struct {
	port.NATSCredentialIssuer
	mu   sync.Mutex
	jwts []string
}

func (c *capturingIssuer) IssueWorkerCredential(ctx context.Context, in port.IssueCredentialInput) (port.IssueCredentialOutput, error) {
	out, err := c.NATSCredentialIssuer.IssueWorkerCredential(ctx, in)
	if err == nil {
		c.mu.Lock()
		c.jwts = append(c.jwts, out.UserJWT)
		c.mu.Unlock()
	}
	return out, err
}

func (c *capturingIssuer) minted() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.jwts)
}

// logRecorder keeps the attributes of every log record.
type logRecorder struct {
	mu      sync.Mutex
	records []map[string]string
}

func (l *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecorder) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *logRecorder) WithGroup(string) slog.Handler            { return l }
func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	m := map[string]string{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool { m[a.Key] = a.Value.String(); return true })
	l.mu.Lock()
	l.records = append(l.records, m)
	l.mu.Unlock()
	return nil
}

func (l *logRecorder) find(msg string) []map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]string
	for _, r := range l.records {
		if r["msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

// calloutEnv is an embedded nats-server running docker/nats-auth-callout.conf,
// with core's auth-callout subscriber wired as in cmd/server/wire.go.
type calloutEnv struct {
	srv         *server.Server
	core        *Bus
	enrollments *fakeEnrollments
	issuer      *capturingIssuer
	create      *enrollment.CreateWorkerEnrollment
	logs        *logRecorder
}

const calloutCorePassword = "core-secret"

func newCalloutEnv(t *testing.T) *calloutEnv {
	t.Helper()
	ctx := context.Background()
	box, err := auth.NewSecretBoxFromMasterKey("auth-callout-test-master-key-material-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	keys := &fakeSigningKeys{}
	sk, err := BootstrapAccountKey(ctx, keys, box, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CAIRN_NATS_AUTH_ISSUER", sk.PublicKey)
	t.Setenv("CAIRN_NATS_PASSWORD", calloutCorePassword)
	opts, err := server.ProcessConfigFile("../../../../docker/nats-auth-callout.conf")
	if err != nil {
		t.Fatal(err)
	}
	issuer := NewCredentialIssuer(keys, box, "CAIRN")
	env := &calloutEnv{
		srv:         startServer(t, opts),
		enrollments: &fakeEnrollments{},
		issuer:      &capturingIssuer{NATSCredentialIssuer: issuer},
		logs:        &logRecorder{},
	}
	env.create = enrollment.NewCreateWorkerEnrollment(env.enrollments, nil, nil, nil)
	nc, err := nats.Connect(env.srv.ClientURL(), nats.UserInfo("cairn-core", calloutCorePassword))
	if err != nil {
		t.Fatal(err)
	}
	if env.core, err = NewBusFromConn(nc, "cairn-server", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(env.core.Close)
	if err := env.core.BootstrapStreams(ctx); err != nil {
		t.Fatal(err)
	}
	proc := enrollment.NewProcessAuthCallout(env.enrollments, env.issuer, noTx{}, nil, nil, time.Hour)
	sub, err := NewAuthCalloutSubscriber(env.core, proc, issuer, slog.New(env.logs)).Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Close(context.Background()) })
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	return env
}

// enroll creates an enrollment through the use case, as the admin API does.
func (e *calloutEnv) enroll(t *testing.T, name, provider string) string {
	t.Helper()
	res, err := e.create.Execute(context.Background(), enrollment.CreateEnrollmentInput{
		Name: name, Provider: provider, ExpiresIn: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res.Token
}

// storeEnrollment writes a row directly, as a pre-v0.3.1 admin API call left it.
func (e *calloutEnv) storeEnrollment(name, provider string) (domain.WorkerEnrollmentID, string) {
	token := "enroll-" + name + "-" + provider
	id := domain.WorkerEnrollmentID(uuid.New())
	e.enrollments.rows = append(e.enrollments.rows, domain.WorkerEnrollment{
		ID:                id,
		TokenHash:         enrollment.HashEnrollmentToken(token),
		Name:              name,
		Provider:          provider,
		Version:           1,
		WorkerNamePattern: "*",
		ExpiresAt:         time.Now().Add(time.Hour),
	})
	return id, token
}

// connectWorker connects like cairn-provider-strava's connectBus: token plus a fresh user nkey.
func (e *calloutEnv) connectWorker(name, token string) (*nats.Conn, error) {
	kp, err := nkeys.CreateUser()
	if err != nil {
		return nil, err
	}
	pub, err := kp.PublicKey()
	if err != nil {
		return nil, err
	}
	return nats.Connect(e.srv.ClientURL(), nats.Token(token), nats.Name(name),
		nats.Nkey(pub, kp.Sign), nats.NoReconnect(),
		nats.ErrorHandler(func(*nats.Conn, *nats.Subscription, error) {}))
}

// permitted reports whether the server accepts op without a permissions violation.
func permitted(t *testing.T, nc *nats.Conn, op func() error) bool {
	t.Helper()
	denied := make(chan struct{}, 1)
	nc.SetErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
		if errors.Is(err, nats.ErrPermissionViolation) {
			select {
			case denied <- struct{}{}:
			default:
			}
		}
	})
	if err := op(); err != nil {
		return false
	}
	if err := nc.Flush(); err != nil {
		return false
	}
	select {
	case <-denied:
		return false
	case <-time.After(300 * time.Millisecond):
		return true
	}
}

func canSubscribe(t *testing.T, nc *nats.Conn, subj string) bool {
	t.Helper()
	return permitted(t, nc, func() error {
		sub, err := nc.SubscribeSync(subj)
		if err == nil {
			t.Cleanup(func() { _ = sub.Unsubscribe() })
		}
		return err
	})
}

func canPublish(t *testing.T, nc *nats.Conn, subj string) bool {
	t.Helper()
	return permitted(t, nc, func() error { return nc.Publish(subj, []byte("{}")) })
}

func TestAuthCalloutAdmitsEnrolledWorkerWithinTemplate(t *testing.T) {
	env := newCalloutEnv(t)
	nc, err := env.connectWorker("w1", env.enroll(t, "w1", "strava"))
	if err != nil {
		t.Fatalf("enrolled worker must be admitted: %v", err)
	}
	worker, err := NewBusFromConn(nc, "w1", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(worker.Close)
	if n := env.enrollments.grantCount(); n != 1 {
		t.Errorf("want one credential grant, got %d", n)
	}

	assertWorkerAllowed(t, env.core, worker)
	assertWorkerDenied(t, worker)
	if !canSubscribe(t, nc, "cairn.jobs.fetch_source.strava") {
		t.Error("own job subject must be allowed")
	}
	for _, subj := range []string{"cairn.tokens.>", "cairn.creds.>", "cairn.jobs.fetch_source.garmin", "cairn.results.>", ">"} {
		if canSubscribe(t, nc, subj) {
			t.Errorf("subscribe %s must be denied", subj)
		}
	}
	if !canPublish(t, nc, "cairn.tokens.strava.get") {
		t.Error("own token subject must be allowed")
	}
	for _, subj := range []string{"cairn.tokens.garmin.get", "cairn.creds.garmin.get", AuthCalloutSubject, "cairn.results.fetch_source.garmin"} {
		if canPublish(t, nc, subj) {
			t.Errorf("publish %s must be denied", subj)
		}
	}
}

// Reproduces the v0.3.0 bug: an enrollment without provider got a user JWT
// with empty allow lists, which NATS treats as unrestricted.
func TestAuthCalloutRejectsEnrollmentWithoutProvider(t *testing.T) {
	env := newCalloutEnv(t)
	id, token := env.storeEnrollment("w1", "")
	if nc, err := env.connectWorker("w1", token); err == nil {
		defer nc.Close()
		for _, subj := range []string{"cairn.tokens.>", "cairn.creds.>"} {
			if canSubscribe(t, nc, subj) {
				t.Errorf("worker without provider could subscribe to %s", subj)
			}
		}
		t.Error("enrollment without provider must not be admitted")
	}
	if minted := env.issuer.minted(); len(minted) != 0 {
		t.Errorf("no user JWT may be minted, got %d", len(minted))
	}
	if n := env.enrollments.grantCount(); n != 0 {
		t.Errorf("no credential grant may be recorded, got %d", n)
	}
	rej := env.logs.find("auth rejected")
	if len(rej) != 1 || rej[0]["reason"] != "enrollment_missing_provider" || rej[0]["enrollment_id"] != id.String() {
		t.Errorf("want one enrollment_missing_provider rejection logged with the enrollment id, got %v", rej)
	}
}

func TestAuthCalloutRejectsUnknownToken(t *testing.T) {
	env := newCalloutEnv(t)
	if nc, err := env.connectWorker("w1", "not-a-token"); err == nil {
		nc.Close()
		t.Fatal("unknown token must not be admitted")
	}
	if rej := env.logs.find("auth rejected"); len(rej) != 1 || rej[0]["reason"] != "unknown_token" {
		t.Errorf("rejections = %v", rej)
	}
}

func TestIssueWorkerCredential(t *testing.T) {
	ctx := context.Background()
	box, err := auth.NewSecretBoxFromMasterKey("auth-callout-test-master-key-material-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	keys := &fakeSigningKeys{}
	sk, err := BootstrapAccountKey(ctx, keys, box, nil)
	if err != nil {
		t.Fatal(err)
	}
	ukp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	upub, err := ukp.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	strava := domain.StandardPermissionTemplate("strava", "w1")
	issuer := NewCredentialIssuer(keys, box, "CAIRN")
	for name, perms := range map[string]domain.WorkerPermissionTemplate{
		"no provider":  domain.StandardPermissionTemplate("", "w1"),
		"no publish":   {AllowSubscribe: strava.AllowSubscribe},
		"no subscribe": {AllowPublish: strava.AllowPublish},
	} {
		_, err := issuer.IssueWorkerCredential(ctx, port.IssueCredentialInput{UserNKeyPublic: upub, Permissions: perms})
		var verr *port.IssueValidationError
		if !errors.As(err, &verr) || verr.Field != "Permissions" {
			t.Errorf("%s: want permissions validation error, got %v", name, err)
		}
	}
	out, err := issuer.IssueWorkerCredential(ctx, port.IssueCredentialInput{UserNKeyPublic: upub, Permissions: strava})
	if err != nil {
		t.Fatalf("standard template: %v", err)
	}
	uc, err := jwt.DecodeUserClaims(out.UserJWT)
	if err != nil {
		t.Fatal(err)
	}
	if uc.Issuer != sk.PublicKey || uc.IssuerAccount != "" || uc.Audience != "CAIRN" || uc.Subject != upub {
		t.Errorf("claims iss=%s issuer_account=%q aud=%q sub=%s", uc.Issuer, uc.IssuerAccount, uc.Audience, uc.Subject)
	}
	if _, err := NewCredentialIssuer(keys, box, "").IssueWorkerCredential(ctx, port.IssueCredentialInput{
		UserNKeyPublic: upub, Permissions: strava,
	}); err == nil {
		t.Error("issuer without worker account must refuse")
	}
}

// A provider outside the built-in list is admitted, scoped to its own subjects only.
func TestAuthCalloutScopesUnregisteredProvider(t *testing.T) {
	env := newCalloutEnv(t)
	nc, err := env.connectWorker("p1", env.enroll(t, "p1", "polar"))
	if err != nil {
		t.Fatalf("polar worker must be admitted: %v", err)
	}
	worker, err := NewBusFromConn(nc, "p1", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(worker.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ps, err := worker.Pull(ctx, port.ConsumerConfig{Stream: StreamJobs, Durable: "polar__fetch", Subject: "cairn.jobs.fetch_source.polar"})
	if err != nil {
		t.Fatalf("pull own jobs: %v", err)
	}
	if err := env.core.Publish(ctx, "cairn.jobs.fetch_source.polar", "", []byte("job")); err != nil {
		t.Fatal(err)
	}
	if msgs, err := ps.Fetch(ctx, 1); err != nil || len(msgs) != 1 {
		t.Fatalf("fetch own job: %v (%d msgs)", err, len(msgs))
	}

	for _, subj := range []string{"cairn.jobs.parse_blob.polar", "cairn.cmd.worker.p1-polar.restart", "cairn.webhooks.p1-polar.event"} {
		if !canSubscribe(t, nc, subj) {
			t.Errorf("subscribe %s must be allowed", subj)
		}
	}
	for _, subj := range []string{"cairn.tokens.polar.get", "cairn.creds.polar.get", "cairn.results.fetch_source.polar.p1", "cairn.accounts.lookup.polar.p1", "cairn.workers.p1-polar.heartbeat"} {
		if !canPublish(t, nc, subj) {
			t.Errorf("publish %s must be allowed", subj)
		}
	}
	// Results are per pool: neither the pre-0.5 provider subject nor another pool's.
	for _, subj := range []string{"cairn.results.fetch_source.polar", "cairn.results.fetch_source.polar.p2", "cairn.accounts.lookup_by_provider_ext"} {
		if canPublish(t, nc, subj) {
			t.Errorf("publish %s must be denied", subj)
		}
	}
	for _, subj := range []string{"cairn.jobs.fetch_source.strava", "cairn.tokens.strava.>", "cairn.cmd.worker.w1-strava.>", "cairn.tokens.>", ">"} {
		if canSubscribe(t, nc, subj) {
			t.Errorf("subscribe %s must be denied", subj)
		}
	}
	for _, subj := range []string{"cairn.tokens.strava.get", "cairn.creds.strava.get", "cairn.jobs.fetch_source.strava", "cairn.results.fetch_source.strava", "cairn.workers.w1-strava.heartbeat"} {
		if canPublish(t, nc, subj) {
			t.Errorf("publish %s must be denied", subj)
		}
	}
	short, cancelShort := context.WithTimeout(context.Background(), time.Second)
	defer cancelShort()
	if _, err := worker.Pull(short, port.ConsumerConfig{Stream: StreamJobs, Durable: "steal", Subject: "cairn.jobs.fetch_source.strava"}); err == nil {
		t.Error("consumer on strava's jobs must be denied")
	}
}

// Malformed provider ids never reach the permission template.
func TestAuthCalloutEnrollRejectsMalformedProvider(t *testing.T) {
	env := newCalloutEnv(t)
	for _, p := range []string{"polar.>", "*", ">", "po.lar", "Polar", ""} {
		if _, err := env.create.Execute(context.Background(), enrollment.CreateEnrollmentInput{
			Name: "w1", Provider: p, ExpiresIn: time.Hour,
		}); !errors.Is(err, enrollment.ErrInvalidInput) {
			t.Errorf("provider %q: err = %v, want ErrInvalidInput", p, err)
		}
	}
}
