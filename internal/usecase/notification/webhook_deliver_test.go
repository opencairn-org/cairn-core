package notification

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// fakeWebhookRepo implements the dispatch-relevant subset of
// port.WebhookEndpointRepo (ListEnabledForUser + RecordDeliveryResult); the
// CRUD methods are unused by the dispatcher and return zero values.
type fakeWebhookRepo struct {
	mu        sync.Mutex
	endpoints []domain.WebhookEndpoint
	results   []recorded
}

type recorded struct {
	id   domain.WebhookEndpointID
	code int
	err  string
}

func (f *fakeWebhookRepo) Create(context.Context, domain.WebhookEndpoint) (domain.WebhookEndpoint, error) {
	return domain.WebhookEndpoint{}, nil
}
func (f *fakeWebhookRepo) Update(context.Context, domain.WebhookEndpoint) error { return nil }
func (f *fakeWebhookRepo) RotateSecret(context.Context, domain.WebhookEndpointID, domain.UserID, string) error {
	return nil
}
func (f *fakeWebhookRepo) Delete(context.Context, domain.WebhookEndpointID, domain.UserID) error {
	return nil
}
func (f *fakeWebhookRepo) GetByID(context.Context, domain.WebhookEndpointID, domain.UserID) (domain.WebhookEndpoint, error) {
	return domain.WebhookEndpoint{}, nil
}
func (f *fakeWebhookRepo) ListForUser(context.Context, domain.UserID) ([]domain.WebhookEndpoint, error) {
	return f.endpoints, nil
}
func (f *fakeWebhookRepo) ListEnabledForUser(context.Context, domain.UserID) ([]domain.WebhookEndpoint, error) {
	return f.endpoints, nil
}
func (f *fakeWebhookRepo) RecordDeliveryResult(_ context.Context, id domain.WebhookEndpointID, code int, derr string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results = append(f.results, recorded{id, code, derr})
	return nil
}

func webhookNotif(t domain.NotificationType) domain.Notification {
	nid, _ := uuid.NewV7()
	return domain.Notification{ID: domain.NotificationID(nid), UserID: domain.UserID(uuid.New()), Type: t, Severity: domain.NotificationSeverityInfo}
}

// fakeSender records each POST and answers with a fixed status / error.
type fakeSender struct {
	code  int
	err   error
	sends []sent
}

type sent struct {
	url     string
	body    []byte
	headers map[string]string
}

var _ port.WebhookSender = (*fakeSender)(nil)

func (f *fakeSender) Send(_ context.Context, url string, body []byte, headers map[string]string) (int, error) {
	f.sends = append(f.sends, sent{url, body, headers})
	return f.code, f.err
}

func deliverWithWebhooks(repo *fakeWebhookRepo, sender *fakeSender) *DeliverNotifications {
	// email nil → only the webhook pass runs; users/prefs unused.
	return NewDeliverNotifications(nil, nil, nil, nil, repo, sender, nil, nil, "http://x", nil)
}

func TestSubscribes_TypeAndSeverityGating(t *testing.T) {
	ep := domain.WebhookEndpoint{Enabled: true, MinSeverity: domain.NotificationSeverityWarn}
	if !ep.Subscribes(domain.NotificationTypeWorkerOffline, domain.NotificationSeverityError) {
		t.Fatal("empty EventTypes + error >= warn should subscribe")
	}
	if ep.Subscribes(domain.NotificationTypeSegmentPersonalRecord, domain.NotificationSeverityInfo) {
		t.Fatal("info < warn must not subscribe")
	}
	ep.EventTypes = []domain.NotificationType{domain.NotificationTypeSegmentPersonalRecord}
	ep.MinSeverity = domain.NotificationSeverityInfo
	if ep.Subscribes(domain.NotificationTypeWorkerOffline, domain.NotificationSeverityInfo) {
		t.Fatal("type not in EventTypes must not subscribe")
	}
	if !ep.Subscribes(domain.NotificationTypeSegmentPersonalRecord, domain.NotificationSeverityInfo) {
		t.Fatal("matching type should subscribe")
	}
	ep.Enabled = false
	if ep.Subscribes(domain.NotificationTypeSegmentPersonalRecord, domain.NotificationSeverityInfo) {
		t.Fatal("disabled endpoint must not subscribe")
	}
}

func TestDeliverWebhook_SignsAndRecords(t *testing.T) {
	const secret = "whsec_test"
	sender := &fakeSender{code: 200}
	repo := &fakeWebhookRepo{endpoints: []domain.WebhookEndpoint{{
		ID: domain.WebhookEndpointID(uuid.New()), Enabled: true,
		URL: "https://hook.example", SigningSecret: secret, MinSeverity: domain.NotificationSeverityInfo,
	}}}
	deliverWithWebhooks(repo, sender).Execute(context.Background(),
		[]domain.Notification{webhookNotif(domain.NotificationTypeSegmentPersonalRecord)})

	if len(sender.sends) != 1 || len(sender.sends[0].body) == 0 || sender.sends[0].url != "https://hook.example" {
		t.Fatalf("expected one POST with a body, got %+v", sender.sends)
	}
	got := sender.sends[0]
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(got.body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got.headers["X-Cairn-Signature"] != want {
		t.Fatalf("signature mismatch:\n got %s\nwant %s", got.headers["X-Cairn-Signature"], want)
	}
	if got.headers["X-Cairn-Event"] != "segment_personal_record" || got.headers["Content-Type"] != "application/json" {
		t.Fatalf("headers = %v", got.headers)
	}
	if len(repo.results) != 1 || repo.results[0].code != 200 || repo.results[0].err != "" {
		t.Fatalf("expected one 2xx success recorded, got %+v", repo.results)
	}
}

func TestDeliverWebhook_SeverityFilteredOut(t *testing.T) {
	sender := &fakeSender{code: 200}
	repo := &fakeWebhookRepo{endpoints: []domain.WebhookEndpoint{{
		ID: domain.WebhookEndpointID(uuid.New()), Enabled: true,
		URL: "https://hook.example", SigningSecret: "x", MinSeverity: domain.NotificationSeverityError,
	}}}
	deliverWithWebhooks(repo, sender).Execute(context.Background(),
		[]domain.Notification{webhookNotif(domain.NotificationTypeActivityImported)})

	if len(sender.sends) != 0 {
		t.Fatal("info notification must not reach an error-min endpoint")
	}
	if len(repo.results) != 0 {
		t.Fatalf("no delivery should be recorded, got %+v", repo.results)
	}
}

func TestDeliverWebhook_Non2xxRecordsFailure(t *testing.T) {
	repo := &fakeWebhookRepo{endpoints: []domain.WebhookEndpoint{{
		ID: domain.WebhookEndpointID(uuid.New()), Enabled: true,
		URL: "https://hook.example", SigningSecret: "x", MinSeverity: domain.NotificationSeverityInfo,
	}}}
	deliverWithWebhooks(repo, &fakeSender{code: 500}).Execute(context.Background(),
		[]domain.Notification{webhookNotif(domain.NotificationTypeActivityImported)})

	if len(repo.results) != 1 || repo.results[0].code != 500 || repo.results[0].err == "" {
		t.Fatalf("expected one recorded failure with 500, got %+v", repo.results)
	}
}

func TestDeliverWebhook_TransportErrorRecordsFailure(t *testing.T) {
	repo := &fakeWebhookRepo{endpoints: []domain.WebhookEndpoint{{
		ID: domain.WebhookEndpointID(uuid.New()), Enabled: true,
		URL: "https://hook.example", SigningSecret: "x", MinSeverity: domain.NotificationSeverityInfo,
	}}}
	deliverWithWebhooks(repo, &fakeSender{err: errors.New("blocked address")}).Execute(context.Background(),
		[]domain.Notification{webhookNotif(domain.NotificationTypeActivityImported)})

	if len(repo.results) != 1 || repo.results[0].code != 0 || repo.results[0].err != "blocked address" {
		t.Fatalf("expected one recorded transport failure, got %+v", repo.results)
	}
}

func TestDeliverWebhook_NilRepoNoPanic(t *testing.T) {
	NewDeliverNotifications(nil, nil, nil, nil, nil, nil, nil, nil, "http://x", nil).
		Execute(context.Background(), []domain.Notification{webhookNotif(domain.NotificationTypeActivityImported)})
}
