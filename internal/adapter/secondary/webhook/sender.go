// Package webhook implements port.WebhookSender over an SSRF-guarded client.
package webhook

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/safehttp"
)

// requestTimeout bounds one POST; the client timeout is a backstop.
const requestTimeout = 10 * time.Second

// Sender POSTs webhook payloads.
type Sender struct{ client *http.Client }

var _ port.WebhookSender = (*Sender)(nil)

// New returns a sender whose dialer blocks internal addresses (safehttp).
func New() *Sender { return &Sender{client: safehttp.NewClient(15 * time.Second)} }

// NewWithClient uses the given client; tests use it to reach loopback servers.
func NewWithClient(c *http.Client) *Sender { return &Sender{client: c} }

// Send POSTs body with the given headers and returns the response status.
func (s *Sender) Send(ctx context.Context, url string, body []byte, headers map[string]string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}
