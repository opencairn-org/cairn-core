package port

import "context"

// WebhookSender POSTs a body to a user-configured URL. Implementations must
// refuse internal targets (SSRF). It returns the response status, 0 on a
// transport error; non-2xx is not an error — callers classify it.
type WebhookSender interface {
	Send(ctx context.Context, url string, body []byte, headers map[string]string) (int, error)
}
