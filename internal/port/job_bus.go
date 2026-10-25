package port

import (
	"context"
	"time"
)

// JobBus is the asynchronous message bus interface. NATS adapter lives
// under internal/adapter/secondary/nats/. Use cases depend on JobBus,
// not on nats.Conn directly.
//
// The name "JobBus" rather than "MessageQueue" is intentional. Cairn
// does not pretend other backends (Kafka, SQS, RabbitMQ) are drop-in
// replacements — their stream model, ACL story, durability semantics,
// and operator UX differ enough that swapping would require a
// different interface anyway. Hexagonal architecture doesn't mean
// "every adapter is swap-able"; it means business logic has no idea
// about the transport.
//
// See docs/architecture.md sections 1-5 for the full NATS-specific
// design (subject naming, stream configs, KV buckets, delivery
// semantics, subject ACLs).
type JobBus interface {
	// Publish enqueues a message. msgID makes the publish idempotent
	// within the stream's dedup window — pass a deterministic ID like
	// "fetch:strava:<account_id>:<ext_id>" for retry-safe publishing.
	Publish(ctx context.Context, subject string, msgID string, body []byte, opts ...PublishOpt) error

	// Subscribe registers a durable push consumer. The server uses this
	// for result-routing and event-processing. handler runs
	// synchronously; returning nil ACKs, returning a NakWithDelayError
	// NAKs with delay, returning any other error NAKs with the consumer's
	// default backoff schedule, and returning a TerminalError marks the
	// message as undeliverable (skips remaining redeliveries, lands in DLQ).
	Subscribe(ctx context.Context, cfg ConsumerConfig, handler MessageHandler) (Subscription, error)

	// Pull registers a pull consumer. Workers use this — they fetch in
	// their own loop with explicit batch sizes for backpressure control.
	Pull(ctx context.Context, cfg ConsumerConfig) (PullSubscription, error)

	// Request is sync request/reply. Used for token-fetch and
	// presign-URL flows. Rare — most communication is fire-and-forget.
	Request(ctx context.Context, subject string, body []byte, timeout time.Duration) ([]byte, error)

	// RespondTo registers a request/reply handler. The server uses this
	// for cairn.tokens.<provider>.fetch and cairn.blobs.presign_*.> .
	RespondTo(ctx context.Context, subject string, handler RequestHandler) (Subscription, error)
	// RespondToSubject is RespondTo for a wildcard subject.
	RespondToSubject(ctx context.Context, subject string, handler SubjectRequestHandler) (Subscription, error)

	// KV returns a handle on the named KV bucket.
	KV(bucket string) (KV, error)

	// ObjectStore returns a handle on the named OS bucket.
	ObjectStore(bucket string) (ObjectStore, error)
}

// PublishOpt is for future extensions (priority, expiry). Empty in v1.
type PublishOpt interface{ apply() }

type ConsumerConfig struct {
	Stream          string        // e.g., "CAIRN_RESULTS"
	Durable         string        // durable consumer name; persists across restarts
	Subject         string        // subject filter, supports wildcards
	QueueGroup      string        // for load balancing in pull mode
	AckWait         time.Duration // how long to wait for ACK before redeliver
	MaxDeliver      int           // max redeliveries before DLQ via advisory
	DeliverPolicy   DeliverPolicy
	BackoffSchedule []time.Duration // per-attempt delay schedule
}

type DeliverPolicy string

const (
	DeliverAll             DeliverPolicy = "all"
	DeliverNew             DeliverPolicy = "new"
	DeliverLast            DeliverPolicy = "last"
	DeliverByStartSequence DeliverPolicy = "by_start_sequence"
)

type MessageHandler func(ctx context.Context, msg Message) error
type RequestHandler func(ctx context.Context, body []byte) ([]byte, error)

// SubjectRequestHandler is a RequestHandler that also sees the concrete
// subject a request arrived on (for wildcard responders).
type SubjectRequestHandler func(ctx context.Context, subject string, body []byte) ([]byte, error)

type Message struct {
	Subject         string
	Headers         map[string]string
	Body            []byte
	DeliveryAttempt int
}

// Subscription is the handle the caller uses to drain/close the subscription.
type Subscription interface {
	Close(ctx context.Context) error
}

// PullSubscription extends Subscription with explicit Fetch semantics.
type PullSubscription interface {
	Subscription
	Fetch(ctx context.Context, batchSize int) ([]PullMessage, error)
}

// PullMessage wraps a Message with the ack-control methods the worker
// uses to drive redelivery.
type PullMessage interface {
	Message() Message
	Ack(ctx context.Context) error
	Nak(ctx context.Context) error
	NakWithDelay(ctx context.Context, delay time.Duration) error
	Term(ctx context.Context) error       // permanent failure, no redelivery
	InProgress(ctx context.Context) error // extend AckWait for long-running work
}

// NakWithDelayError instructs the push-consumer adapter to NAK with the
// given delay. Used as a handler return value to control backoff per-message.
type NakWithDelayError struct {
	Reason string // stable identifier for metrics ("rate_limited", "transient_provider")
	Delay  time.Duration
	Cause  error
}

func (e *NakWithDelayError) Error() string {
	switch {
	case e.Reason != "" && e.Cause != nil:
		return e.Reason + ": " + e.Cause.Error()
	case e.Reason != "":
		return e.Reason
	case e.Cause != nil:
		return e.Cause.Error()
	default:
		return "nak-with-delay"
	}
}
func (e *NakWithDelayError) Unwrap() error { return e.Cause }

// TerminalError marks a message as permanently failed. The push-consumer
// adapter will Term() the message, sending it to the DLQ on the next
// advisory.
type TerminalError struct {
	Reason string
	Cause  error
}

func (e *TerminalError) Error() string {
	switch {
	case e.Reason != "" && e.Cause != nil:
		return e.Reason + ": " + e.Cause.Error()
	case e.Reason != "":
		return e.Reason
	case e.Cause != nil:
		return e.Cause.Error()
	default:
		return "terminal-error"
	}
}
func (e *TerminalError) Unwrap() error { return e.Cause }

// ---------------------------------------------------------------------------
// KV
// ---------------------------------------------------------------------------

// ErrKVKeyNotFound is returned by KV.Get when no value exists for the
// key. Adapters MUST translate their underlying backend's not-found
// error into this sentinel so use cases can use errors.Is for the
// distinction without coupling to NATS or any other backend.
var ErrKVKeyNotFound = errInternal("kv: key not found")

type KV interface {
	Get(ctx context.Context, key string) (KVEntry, error)
	// Keys lists all keys currently in the bucket.
	Keys(ctx context.Context) ([]string, error)
	Put(ctx context.Context, key string, value []byte) (revision uint64, err error)
	// CompareAndSet writes value only if the current revision matches.
	// Returns (newRev, false, nil) on revision mismatch — caller should retry.
	CompareAndSet(ctx context.Context, key string, value []byte, expectedRev uint64) (revision uint64, ok bool, err error)
	Delete(ctx context.Context, key string) error
	Watch(ctx context.Context, key string) (KVWatcher, error)
}

type KVEntry struct {
	Key       string
	Value     []byte
	Revision  uint64
	CreatedAt time.Time
}

type KVWatcher interface {
	Updates() <-chan KVEntry
	Close(ctx context.Context) error
}

// ---------------------------------------------------------------------------
// ObjectStore (transient oversized message spillover; see docs/architecture.md §9)
// ---------------------------------------------------------------------------

type ObjectStore interface {
	Put(ctx context.Context, key string, data []byte, meta ObjectMeta) error
	Get(ctx context.Context, key string) ([]byte, ObjectMeta, error)
	Delete(ctx context.Context, key string) error
}

type ObjectMeta struct {
	ContentType string
	SizeBytes   int64
	Headers     map[string]string
}
