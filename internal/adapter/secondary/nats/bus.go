// Package nats provides the NATS+JetStream implementations of
// port.JobBus, port.RateLimiter, and port.NATSCredentialIssuer, plus
// the auth-callout subscriber that drives port.WorkerEnrollmentRepo via
// the enrollment use cases.
//
// Imports of nats.go are confined to this package — domain and port
// stay free of nats.* types. The Bus is the only object the rest of
// the binary sees; everything else is reached through port interfaces.
//
// See docs/architecture.md §1–§5 for the design behind subject layout,
// stream configs, KV buckets, delivery semantics, and the JobBus
// interface this file implements.
package nats

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/opencairn-org/cairn-core/internal/config"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Bus is the NATS-backed JobBus adapter. Wraps a single *nats.Conn and
// the jetstream.JetStream context on top of it.
//
// Lifecycle:
//
//	bus, err := nats.NewBus(ctx, cfg.NATS, logger)
//	defer bus.Close()
//	bus.BootstrapStreams(ctx)   // idempotent; safe to call on every start
//	// pass `bus` as port.JobBus into use-case wiring
type Bus struct {
	cfg    config.NATSConfig
	logger *slog.Logger

	conn *nats.Conn
	js   jetstream.JetStream

	// kvs and oss are caches of resolved KeyValue/ObjectStore handles
	// so repeated KV("...") calls don't re-resolve every time.
	mu  sync.Mutex
	kvs map[string]jetstream.KeyValue
	oss map[string]jetstream.ObjectStore

	// routes caches per-subject stream lookups for Publish.
	routes      map[string]route
	noStreamTTL time.Duration
}

// defaultNoStreamTTL bounds how long a "no stream" lookup is trusted.
const defaultNoStreamTTL = time.Minute

// NewBusFromConn wraps an already-connected *nats.Conn into a Bus.
// Used by workers, which build their own connection with the enrollment
// token before the auth-callout admits them. The Bus
// takes ownership of the connection — caller should not Close it
// directly; instead call Bus.Close which drains + closes.
//
// Unlike NewBus, this does NOT trigger BootstrapStreams. Workers don't
// declare streams; the server does.
func NewBusFromConn(nc *nats.Conn, clientName string, logger *slog.Logger) (*Bus, error) {
	if logger == nil {
		logger = slog.Default()
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("nats: create jetstream context: %w", err)
	}
	return &Bus{
		cfg: config.NATSConfig{
			URL:        nc.ConnectedUrl(),
			ClientName: clientName,
			// A fetch_source job makes up to two Strava HTTP calls (30s timeout
			// each) plus rate-limit waits, so the NATS default 30s AckWait would
			// redeliver mid-processing. Give handlers room. MaxDeliver stays
			// unbounded: the handler Terms genuine poison (bad payload /
			// needs_reauth) immediately, while transient + rate-limited errors
			// should retry until they resolve rather than DLQ a real activity.
			JobAckWait: 2 * time.Minute,
		},
		logger: logger.With("component", "nats_bus"),
		conn:   nc,
		js:     js,
		kvs:    map[string]jetstream.KeyValue{},
		oss:    map[string]jetstream.ObjectStore{},
		routes: map[string]route{},
		// Streams may appear after we connect; recheck misses.
		noStreamTTL: defaultNoStreamTTL,
	}, nil
}

// NewBus dials the NATS server and constructs a JetStream context.
// Connection is established eagerly (no lazy mode) so config errors are
// surfaced at startup rather than at first use.
func NewBus(ctx context.Context, cfg config.NATSConfig, logger *slog.Logger) (*Bus, error) {
	if logger == nil {
		logger = slog.Default()
	}
	opts, err := buildConnectOpts(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("nats: build connect opts: %w", err)
	}
	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("nats: connect %s: %w", cfg.URL, err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("nats: create jetstream context: %w", err)
	}
	logger.Info("nats: connected",
		"url", cfg.URL,
		"client_name", cfg.ClientName,
		"cluster", cfg.ClusterName,
	)
	return &Bus{
		cfg:    cfg,
		logger: logger.With("component", "nats_bus"),
		conn:   nc,
		js:     js,
		kvs:    map[string]jetstream.KeyValue{},
		oss:    map[string]jetstream.ObjectStore{},
		routes: map[string]route{},
		// Streams may appear after we connect; recheck misses.
		noStreamTTL: defaultNoStreamTTL,
	}, nil
}

// Close drains pending messages and tears down the connection.
// Safe to call multiple times.
func (b *Bus) Close() {
	if b.conn == nil {
		return
	}
	// Drain blocks until pending publishes flush and subscriptions
	// finish processing. Bounded by the connection drain timeout.
	if err := b.conn.Drain(); err != nil {
		b.logger.Warn("nats: drain failed", "error", err)
	}
	// After Drain, Close releases socket + goroutines.
	b.conn.Close()
	b.conn = nil
}

// Conn returns the underlying *nats.Conn for adapters within this package
// that need it (e.g., the auth-callout subscriber uses core NATS,
// not JetStream). NOT exposed via port.JobBus — only intra-package use.
func (b *Bus) Conn() *nats.Conn { return b.conn }

// StoredMsg reads one message still held by stream (for dead-letter capture).
func (b *Bus) StoredMsg(ctx context.Context, stream string, seq uint64) (subject string, data []byte, headers map[string]string, err error) {
	st, err := b.js.Stream(ctx, stream)
	if err != nil {
		return "", nil, nil, fmt.Errorf("nats: stream %s: %w", stream, err)
	}
	m, err := st.GetMsg(ctx, seq)
	if err != nil {
		return "", nil, nil, fmt.Errorf("nats: get %s#%d: %w", stream, seq, err)
	}
	headers = make(map[string]string, len(m.Header))
	for k := range m.Header {
		headers[k] = m.Header.Get(k)
	}
	return m.Subject, m.Data, headers, nil
}

// JS returns the jetstream context. Same constraint as Conn() —
// intra-package use only.
func (b *Bus) JS() jetstream.JetStream { return b.js }

// ---------------------------------------------------------------------------
// port.JobBus: Publish
// ---------------------------------------------------------------------------

// Publish sends via JetStream if a stream covers the subject, else via core NATS.
// Decided up front: a JS publish to a core subscriber awaits an ack that never comes.
// msgID sets Nats-Msg-Id so JetStream dedup collapses retries.
func (b *Bus) Publish(
	ctx context.Context,
	subject string,
	msgID string,
	body []byte,
	_ ...port.PublishOpt,
) error {
	msg := &nats.Msg{
		Subject: subject,
		Data:    body,
		Header:  nats.Header{},
	}
	if msgID != "" {
		msg.Header.Set(jetstream.MsgIDHeader, msgID)
	}
	stream, err := b.streamFor(ctx, subject)
	if err != nil {
		return err
	}
	if !stream {
		return b.corePublish(msg)
	}
	_, err = b.js.PublishMsg(ctx, msg)
	if err == nil {
		return nil
	}
	if errors.Is(err, nats.ErrNoStreamResponse) || errors.Is(err, jetstream.ErrNoStreamResponse) {
		// Stream deleted since the lookup and nobody listens.
		b.forgetRoute(subject)
		return b.corePublish(msg)
	}
	return fmt.Errorf("nats: js publish %s: %w", subject, err)
}

func (b *Bus) corePublish(msg *nats.Msg) error {
	if err := b.conn.PublishMsg(msg); err != nil {
		return fmt.Errorf("nats: core publish %s: %w", msg.Subject, err)
	}
	return nil
}

// route is a cached stream lookup; checked is when a miss was recorded.
type route struct {
	stream  bool
	checked time.Time
}

// streamFor reports whether a stream covers subject. Hits are cached for
// good; misses for noStreamTTL so streams created later are seen.
func (b *Bus) streamFor(ctx context.Context, subject string) (bool, error) {
	b.mu.Lock()
	r, ok := b.routes[subject]
	b.mu.Unlock()
	if ok && (r.stream || time.Since(r.checked) < b.noStreamTTL) {
		return r.stream, nil
	}
	_, err := b.js.StreamNameBySubject(ctx, subject)
	switch {
	case err == nil:
		r = route{stream: true}
	case errors.Is(err, jetstream.ErrStreamNotFound):
		r = route{checked: time.Now()}
	default:
		return false, fmt.Errorf("nats: stream lookup %s: %w", subject, err)
	}
	b.mu.Lock()
	b.routes[subject] = r
	b.mu.Unlock()
	return r.stream, nil
}

func (b *Bus) forgetRoute(subject string) {
	b.mu.Lock()
	delete(b.routes, subject)
	b.mu.Unlock()
}

// ---------------------------------------------------------------------------
// port.JobBus: Subscribe (push consumer)
// ---------------------------------------------------------------------------

// Subscribe creates (or updates) a durable push consumer and dispatches
// messages to `handler`. Returning nil from handler ACKs; returning a
// *port.NakWithDelayError NAKs with the embedded delay; returning a
// *port.TerminalError calls Term() (no more retries, message DLQ'd);
// any other error NAKs with the consumer's default backoff schedule.
func (b *Bus) Subscribe(
	ctx context.Context,
	cfg port.ConsumerConfig,
	handler port.MessageHandler,
) (port.Subscription, error) {
	cons, err := b.resolveConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}

	cc, err := cons.Consume(func(jm jetstream.Msg) {
		b.handlePushMessage(ctx, jm, handler)
	})
	if err != nil {
		return nil, fmt.Errorf("nats: consume on %s: %w", cfg.Subject, err)
	}
	return &pushSubscription{cc: cc, logger: b.logger.With("consumer", cfg.Durable)}, nil
}

// ackExtendInterval is how often a still-processing message's ack deadline is
// extended. Must stay comfortably below the configured JobAckWait.
const ackExtendInterval = 30 * time.Second

func (b *Bus) handlePushMessage(
	ctx context.Context,
	jm jetstream.Msg,
	handler port.MessageHandler,
) {
	// A slow handler (ingest + recluster + follow-ups on a heavy activity) can
	// outlive AckWait; without extending the deadline JetStream redelivers
	// mid-processing and the same result is ingested again on every cycle.
	hbDone := make(chan struct{})
	defer close(hbDone)
	go b.natsExtendAckLoop(jm, hbDone)

	pm := wrapMessage(jm)
	err := b.runHandler(ctx, jm, pm, handler)
	if err == nil {
		if ackErr := jm.Ack(); ackErr != nil {
			b.logger.Warn("nats: ack failed", "subject", jm.Subject(), "error", ackErr)
		}
		return
	}

	b.natsSettleFailed(jm, err)
}

// runHandler calls handler, turning a panic into a retryable error so one bad
// message cannot take the server down.
func (b *Bus) runHandler(ctx context.Context, jm jetstream.Msg, pm port.Message, handler port.MessageHandler) (err error) {
	defer func() {
		if p := recover(); p != nil {
			b.logger.Error("nats: handler panicked", "subject", jm.Subject(), "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("handler panic: %v", p)
		}
	}()
	return handler(ctx, pm)
}

// Redelivery back-off for transient failures: retryBase·3^(n-1), capped, so
// MaxDeliver attempts span minutes (a DB restart) rather than milliseconds.
var (
	retryBase = 5 * time.Second
	retryMax  = 5 * time.Minute
)

// retryDelay is the back-off before delivery n+1, after n failed deliveries.
func retryDelay(delivered uint64) time.Duration {
	d := retryBase
	for i := uint64(1); i < delivered && d < retryMax; i++ {
		d *= 3
	}
	return min(d, retryMax)
}

// natsExtendAckLoop extends jm's ack deadline every ackExtendInterval until done closes.
func (b *Bus) natsExtendAckLoop(jm jetstream.Msg, done <-chan struct{}) {
	t := time.NewTicker(ackExtendInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if err := jm.InProgress(); err != nil {
				b.logger.Debug("nats: ack-deadline extend failed", "subject", jm.Subject(), "error", err)
				return
			}
		}
	}
}

// natsSettleFailed terms, delayed-naks or naks jm according to the handler error.
func (b *Bus) natsSettleFailed(jm jetstream.Msg, err error) {
	// Distinguish the three failure dispositions:
	var term *port.TerminalError
	var nakDelay *port.NakWithDelayError
	switch {
	case errors.As(err, &term):
		if termErr := jm.TermWithReason(term.Reason); termErr != nil {
			b.logger.Warn("nats: term failed", "subject", jm.Subject(), "error", termErr)
		}
		b.logger.Info("nats: message terminated",
			"subject", jm.Subject(), "reason", term.Reason, "cause", err)
	case errors.As(err, &nakDelay):
		if nakErr := jm.NakWithDelay(nakDelay.Delay); nakErr != nil {
			b.logger.Warn("nats: nak-with-delay failed", "subject", jm.Subject(), "error", nakErr)
		}
	default:
		var delivered uint64 = 1
		if md, mdErr := jm.Metadata(); mdErr == nil {
			delivered = md.NumDelivered
		}
		if nakErr := jm.NakWithDelay(retryDelay(delivered)); nakErr != nil {
			b.logger.Warn("nats: nak failed", "subject", jm.Subject(), "error", nakErr)
		}
	}
}

// ---------------------------------------------------------------------------
// port.JobBus: Pull (pull consumer)
// ---------------------------------------------------------------------------

// Pull creates (or updates) a durable pull consumer that workers fetch
// from. Unlike Subscribe, the caller controls batch size and ack timing
// per message — useful for backpressure-aware workers.
func (b *Bus) Pull(
	ctx context.Context,
	cfg port.ConsumerConfig,
) (port.PullSubscription, error) {
	cons, err := b.resolveConsumer(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &pullSubscription{cons: cons, logger: b.logger.With("consumer", cfg.Durable)}, nil
}

// resolveConsumer is the common path for Subscribe and Pull. Calls
// CreateOrUpdateConsumer so configuration changes (AckWait, MaxDeliver,
// BackoffSchedule) take effect after a restart without manual cleanup.
func (b *Bus) resolveConsumer(
	ctx context.Context,
	cfg port.ConsumerConfig,
) (jetstream.Consumer, error) {
	if cfg.Stream == "" {
		return nil, errors.New("nats: ConsumerConfig.Stream required")
	}
	if cfg.Durable == "" {
		return nil, errors.New("nats: ConsumerConfig.Durable required")
	}

	maxDeliver := cfg.MaxDeliver
	if maxDeliver == 0 {
		maxDeliver = b.cfg.JobMaxDeliver
	}
	ackWait := cfg.AckWait
	if ackWait == 0 {
		ackWait = b.cfg.JobAckWait
	}

	deliverPolicy := jetstream.DeliverAllPolicy
	switch cfg.DeliverPolicy {
	case port.DeliverNew:
		deliverPolicy = jetstream.DeliverNewPolicy
	case port.DeliverLast:
		deliverPolicy = jetstream.DeliverLastPolicy
	case port.DeliverAll, "":
		deliverPolicy = jetstream.DeliverAllPolicy
	}

	cc := jetstream.ConsumerConfig{
		Durable:       cfg.Durable,
		FilterSubject: cfg.Subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       ackWait,
		MaxDeliver:    maxDeliver,
		DeliverPolicy: deliverPolicy,
		BackOff:       cfg.BackoffSchedule,
	}

	cons, err := b.js.CreateOrUpdateConsumer(ctx, cfg.Stream, cc)
	if err != nil {
		return nil, fmt.Errorf("nats: create/update consumer %s on %s: %w",
			cfg.Durable, cfg.Stream, err)
	}
	return cons, nil
}

// ---------------------------------------------------------------------------
// port.JobBus: Request / RespondTo (request-reply)
// ---------------------------------------------------------------------------

// Request sends a request and waits for one reply. Used for the OAuth
// token-fetch and blob-presign flows where the caller wants a synchronous
// answer. Core NATS, not JetStream — replies don't need durability.
func (b *Bus) Request(
	ctx context.Context,
	subject string,
	body []byte,
	timeout time.Duration,
) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	msg, err := b.conn.RequestWithContext(reqCtx, subject, body)
	if err != nil {
		return nil, fmt.Errorf("nats: request %s: %w", subject, err)
	}
	return msg.Data, nil
}

// RespondTo subscribes to `subject` and dispatches each incoming request
// to `handler`; the handler's return value is published as the reply.
// Errors are mapped to a NATS error header so the requester sees a
// specific failure reason.
func (b *Bus) RespondTo(
	ctx context.Context,
	subject string,
	handler port.RequestHandler,
) (port.Subscription, error) {
	return b.RespondToSubject(ctx, subject, func(ctx context.Context, _ string, body []byte) ([]byte, error) {
		return handler(ctx, body)
	})
}

// RespondToSubject registers a request/reply handler on a (wildcard)
// subject; the handler sees the concrete subject of each request.
func (b *Bus) RespondToSubject(
	ctx context.Context,
	subject string,
	handler port.SubjectRequestHandler,
) (port.Subscription, error) {
	sub, err := b.conn.Subscribe(subject, func(msg *nats.Msg) {
		if msg.Reply == "" {
			b.logger.Warn("nats: request without reply subject", "subject", msg.Subject)
			return
		}
		resp, herr := handler(ctx, msg.Subject, msg.Data)
		reply := &nats.Msg{
			Subject: msg.Reply,
			Data:    resp,
			Header:  nats.Header{},
		}
		if herr != nil {
			reply.Header.Set("Nats-Error", herr.Error())
			if len(resp) == 0 {
				reply.Data = []byte(herr.Error())
			}
		}
		if pubErr := b.conn.PublishMsg(reply); pubErr != nil {
			b.logger.Warn("nats: reply publish failed", "subject", msg.Subject, "error", pubErr)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("nats: subscribe %s: %w", subject, err)
	}
	return &coreSubscription{sub: sub}, nil
}

// ---------------------------------------------------------------------------
// port.JobBus: KV / ObjectStore handles
// ---------------------------------------------------------------------------

// KV returns a handle on the named KV bucket. The bucket must already
// exist — call BootstrapStreams (or `nats kv add`) at deploy time.
func (b *Bus) KV(bucket string) (port.KV, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if h, ok := b.kvs[bucket]; ok {
		return &kvHandle{kv: h}, nil
	}
	h, err := b.js.KeyValue(context.Background(), bucket)
	if err != nil {
		return nil, fmt.Errorf("nats: resolve kv %s: %w", bucket, err)
	}
	b.kvs[bucket] = h
	return &kvHandle{kv: h}, nil
}

// ObjectStore returns a handle on the named OS bucket. Similar lifecycle
// to KV — bucket pre-existence is required.
func (b *Bus) ObjectStore(bucket string) (port.ObjectStore, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if h, ok := b.oss[bucket]; ok {
		return &objectStoreHandle{os: h}, nil
	}
	h, err := b.js.ObjectStore(context.Background(), bucket)
	if err != nil {
		return nil, fmt.Errorf("nats: resolve object store %s: %w", bucket, err)
	}
	b.oss[bucket] = h
	return &objectStoreHandle{os: h}, nil
}

// ---------------------------------------------------------------------------
// Connection options
// ---------------------------------------------------------------------------

func buildConnectOpts(cfg config.NATSConfig, logger *slog.Logger) ([]nats.Option, error) {
	opts := []nats.Option{
		nats.Name(cfg.ClientName),
		nats.Timeout(cfg.ConnectTimeout),
		nats.MaxReconnects(cfg.MaxReconnects),
		nats.ReconnectWait(cfg.ReconnectWait),
		nats.RetryOnFailedConnect(true),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				logger.Warn("nats: disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(c *nats.Conn) {
			logger.Info("nats: reconnected", "url", c.ConnectedUrl())
		}),
		nats.ErrorHandler(func(_ *nats.Conn, sub *nats.Subscription, err error) {
			subject := ""
			if sub != nil {
				subject = sub.Subject
			}
			logger.Warn("nats: async error", "subject", subject, "error", err)
		}),
	}

	// Credentials: file > user/password > anonymous.
	switch {
	case cfg.CredsFile != "":
		opts = append(opts, nats.UserCredentials(cfg.CredsFile))
	case cfg.Username != "":
		opts = append(opts, nats.UserInfo(cfg.Username, cfg.Password))
	}

	// TLS.
	if cfg.TLSCAFile != "" || cfg.TLSCertFile != "" {
		tlsCfg, err := buildTLSConfig(cfg)
		if err != nil {
			return nil, err
		}
		opts = append(opts, nats.Secure(tlsCfg))
	}

	return opts, nil
}

func buildTLSConfig(cfg config.NATSConfig) (*tls.Config, error) {
	t := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.TLSCAFile != "" {
		caBytes, err := os.ReadFile(cfg.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("read TLS CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, errors.New("TLS CA file is not valid PEM")
		}
		t.RootCAs = pool
	}
	if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load TLS client cert: %w", err)
		}
		t.Certificates = []tls.Certificate{cert}
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// Subscription wrappers
// ---------------------------------------------------------------------------

type pushSubscription struct {
	cc     jetstream.ConsumeContext
	logger *slog.Logger
}

func (s *pushSubscription) Close(ctx context.Context) error {
	if s.cc != nil {
		s.cc.Stop()
	}
	return nil
}

type pullSubscription struct {
	cons   jetstream.Consumer
	logger *slog.Logger
}

func (s *pullSubscription) Close(_ context.Context) error { return nil }

// Fetch pulls up to `batchSize` messages, blocking briefly (FetchMaxWait)
// if fewer are available. Returns an empty slice on timeout.
func (s *pullSubscription) Fetch(ctx context.Context, batchSize int) ([]port.PullMessage, error) {
	if batchSize <= 0 {
		batchSize = 1
	}
	batch, err := s.cons.Fetch(batchSize, jetstream.FetchMaxWait(5*time.Second))
	if err != nil {
		return nil, fmt.Errorf("nats: fetch: %w", err)
	}

	var out []port.PullMessage
	for jm := range batch.Messages() {
		out = append(out, &pullMessage{jm: jm})
	}
	if err := batch.Error(); err != nil {
		return out, fmt.Errorf("nats: fetch batch: %w", err)
	}
	return out, nil
}

type pullMessage struct {
	jm jetstream.Msg
}

func (m *pullMessage) Message() port.Message { return wrapMessage(m.jm) }

func (m *pullMessage) Ack(_ context.Context) error  { return m.jm.Ack() }
func (m *pullMessage) Nak(_ context.Context) error  { return m.jm.Nak() }
func (m *pullMessage) Term(_ context.Context) error { return m.jm.Term() }

func (m *pullMessage) NakWithDelay(_ context.Context, delay time.Duration) error {
	return m.jm.NakWithDelay(delay)
}

func (m *pullMessage) InProgress(_ context.Context) error { return m.jm.InProgress() }

type coreSubscription struct {
	sub *nats.Subscription
}

func (s *coreSubscription) Close(_ context.Context) error {
	if s.sub != nil {
		return s.sub.Drain()
	}
	return nil
}

// wrapMessage converts a jetstream.Msg into a port.Message snapshot.
// We don't expose the Ack/Nak methods on Message itself — push-handler
// callers don't get to choose; pull-handler callers use PullMessage.
func wrapMessage(jm jetstream.Msg) port.Message {
	headers := map[string]string{}
	for k, vals := range jm.Headers() {
		if len(vals) > 0 {
			headers[k] = vals[0]
		}
	}
	attempt := 0
	if meta, err := jm.Metadata(); err == nil && meta != nil {
		attempt = int(min(meta.NumDelivered, math.MaxInt))
	}
	return port.Message{
		Subject:         jm.Subject(),
		Headers:         headers,
		Body:            jm.Data(),
		DeliveryAttempt: attempt,
	}
}
