package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/opencairn-org/cairn-core/internal/port"
)

// WebhookForwarder relays provider webhooks to the pool named by the worker
// key in the URL path (<name>-<provider>). Core knows no envelope shape,
// signature scheme or handshake: the worker verifies and decodes; core is
// only the transport.
type WebhookForwarder struct {
	Bus port.JobBus
	Log *slog.Logger
	Now func() time.Time
}

// webhookVerifyTimeout bounds the GET handshake; providers expect a fast
// reply, and no connected worker should fail fast.
const webhookVerifyTimeout = 3 * time.Second

var (
	// ErrWebhookForward is a failed publish of the event.
	ErrWebhookForward = errors.New("forward failed")
	// ErrWebhookNoWorker is a verify request nobody answered.
	ErrWebhookNoWorker = errors.New("no worker available to handle verification")
	// ErrWebhookBadReply is an undecodable worker reply.
	ErrWebhookBadReply = errors.New("invalid response from worker")
)

// Wire types mirror workersdk's WebhookEvent / WebhookVerify JSON shapes;
// kept inline so core has no dependency on the SDK.
type webhookEventEnvelope struct {
	// Provider carries the worker key; the wire name predates pools.
	Provider string            `json:"provider"`
	Headers  map[string]string `json:"headers"`
	Body     []byte            `json:"body"`
}

type webhookVerifyRequest struct {
	Method string            `json:"method"`
	Query  map[string]string `json:"query"`
}

// WebhookVerifyReply is the worker's answer to a verification handshake.
type WebhookVerifyReply struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Body        []byte `json:"body"`
}

// ValidWebhookTarget: the worker key must be a safe NATS subject token
// (ASCII alnum, hyphen, underscore); a key is at most name + "-" + provider.
func ValidWebhookTarget(p string) bool {
	if p == "" || len(p) > 97 {
		return false
	}
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

func (f *WebhookForwarder) log() *slog.Logger {
	if f.Log != nil {
		return f.Log
	}
	return slog.Default()
}

// Forward publishes the raw event to cairn.webhooks.<worker_key>.event; the
// headers travel along so the worker can check HMAC signatures. JetStream
// keeps it until a worker connects.
func (f *WebhookForwarder) Forward(ctx context.Context, workerKey string, headers map[string]string, sourceAddr string, body []byte) error {
	now := time.Now()
	if f.Now != nil {
		now = f.Now()
	}
	env := webhookEventEnvelope{Provider: workerKey, Headers: headers, Body: body}
	if env.Headers == nil {
		env.Headers = map[string]string{}
	}
	env.Headers["x-webhook-source-addr"] = sourceAddr
	env.Headers["x-webhook-received-at"] = now.UTC().Format(time.RFC3339)
	payload, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("encode envelope: %w", err)
	}
	subject := "cairn.webhooks." + workerKey + ".event"
	// Dedup only a byte-identical redelivery within the same second: the
	// body hash keeps two distinct events of one second apart, the second
	// keeps a legitimate repeat (same payload, later change) deliverable.
	sum := sha256.Sum256(body)
	msgID := workerKey + ":" + hex.EncodeToString(sum[:16]) + ":" + env.Headers["x-webhook-received-at"]
	// Bounded so an unauthenticated webhook can't hang on a slow JetStream.
	pubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := f.Bus.Publish(pubCtx, subject, msgID, payload); err != nil {
		f.log().Warn("forward webhook event failed", "worker_key", workerKey, "error", err)
		return ErrWebhookForward
	}
	f.log().Info("webhook event forwarded", "worker_key", workerKey, "subject", subject, "size_bytes", len(body))
	return nil
}

// Verify relays a GET handshake to cairn.webhooks.<worker_key>.verify and
// returns the worker's reply, defaulted to 200 application/json.
func (f *WebhookForwarder) Verify(ctx context.Context, workerKey, method string, query map[string]string) (WebhookVerifyReply, error) {
	payload, err := json.Marshal(webhookVerifyRequest{Method: method, Query: query})
	if err != nil {
		return WebhookVerifyReply{}, fmt.Errorf("encode verify request: %w", err)
	}
	subject := "cairn.webhooks." + workerKey + ".verify"
	ctx, cancel := context.WithTimeout(ctx, webhookVerifyTimeout)
	defer cancel()
	respBytes, err := f.Bus.Request(ctx, subject, payload, webhookVerifyTimeout)
	if err != nil {
		f.log().Warn("verify request to worker failed", "worker_key", workerKey, "subject", subject, "error", err)
		return WebhookVerifyReply{}, ErrWebhookNoWorker
	}
	var resp WebhookVerifyReply
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		f.log().Warn("decode verify response failed", "worker_key", workerKey, "error", err)
		return WebhookVerifyReply{}, ErrWebhookBadReply
	}
	if resp.Status == 0 {
		resp.Status = 200
	}
	if resp.ContentType == "" {
		resp.ContentType = "application/json"
	}
	return resp, nil
}
