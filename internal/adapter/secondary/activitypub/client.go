// Package activitypub is the outbound ActivityPub HTTP client: remote actor
// fetches, WebFinger and signed inbox delivery.
package activitypub

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/httpsig"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/safehttp"
)

// Client implements port.FederationClient.
type Client struct {
	// HTTP is SSRF-guarded by default.
	HTTP *http.Client
	// Scheme of WebFinger lookups: https per spec.
	Scheme string
}

var _ port.FederationClient = (*Client)(nil)

const timeout = 15 * time.Second

// NewClient returns the SSRF-guarded https client.
func NewClient() *Client {
	return &Client{HTTP: safehttp.NewClient(timeout), Scheme: "https"}
}

// AllowPrivateHosts drops the SSRF dial guard and resolves WebFinger over
// plain http, for trusted LANs and the interop harness. Call before use.
func (c *Client) AllowPrivateHosts() {
	c.HTTP = &http.Client{Timeout: timeout}
	c.Scheme = "http"
}

// remoteActor is the subset of a remote actor document we need.
type remoteActor struct {
	ID                string `json:"id"`
	Inbox             string `json:"inbox"`
	PreferredUsername string `json:"preferredUsername"`
	Endpoints         struct {
		SharedInbox string `json:"sharedInbox"`
	} `json:"endpoints"`
	PublicKey struct {
		PublicKeyPem string `json:"publicKeyPem"`
	} `json:"publicKey"`
}

// FetchActor dereferences actorID.
func (c *Client) FetchActor(ctx context.Context, actorID string) (domain.FederatedActor, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, actorID, nil) //nolint:gosec // G704: SSRF-guarded client; blocked domains rejected by the caller
	if err != nil {
		return domain.FederatedActor{}, err
	}
	req.Header.Set("Accept", "application/activity+json")
	resp, err := c.HTTP.Do(req) //nolint:gosec // G704: SSRF-guarded safehttp client (dial guard blocks private IPs)
	if err != nil {
		return domain.FederatedActor{}, fmt.Errorf("fetch actor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return domain.FederatedActor{}, fmt.Errorf("fetch actor %s: status %d", actorID, resp.StatusCode)
	}
	var ra remoteActor
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ra); err != nil {
		return domain.FederatedActor{}, fmt.Errorf("decode actor: %w", err)
	}
	if ra.Inbox == "" || ra.PublicKey.PublicKeyPem == "" {
		return domain.FederatedActor{}, errors.New("actor missing inbox or public key")
	}
	host := ""
	if u, err := url.Parse(actorID); err == nil {
		host = u.Host
	}
	return domain.FederatedActor{
		ActorID: actorID, Inbox: ra.Inbox, SharedInbox: ra.Endpoints.SharedInbox,
		PublicKeyPEM: ra.PublicKey.PublicKeyPem, PreferredUsername: ra.PreferredUsername,
		Domain: host,
	}, nil
}

// webfingerHost is a DNS name with an optional port: no userinfo, path, query or IP-literal tricks.
var webfingerHost = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*(:[0-9]{1,5})?$`)

// ResolveHandle turns "alice@remote.example" (or "@alice@…") into the actor URL.
func (c *Client) ResolveHandle(ctx context.Context, handle string) (string, error) {
	handle = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(handle), "@"))
	at := strings.LastIndexByte(handle, '@')
	if at <= 0 || at == len(handle)-1 {
		return "", errors.New("expected user@host")
	}
	user, host := handle[:at], handle[at+1:]
	if len(host) > 253 || !webfingerHost.MatchString(host) {
		return "", fmt.Errorf("invalid host %q", host)
	}
	wf := c.Scheme + "://" + host + "/.well-known/webfinger?resource=" + url.QueryEscape("acct:"+user+"@"+host)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wf, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/jrd+json, application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("webfinger: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("webfinger %s: status %d", host, resp.StatusCode)
	}
	var doc struct {
		Links []struct {
			Rel  string `json:"rel"`
			Type string `json:"type"`
			Href string `json:"href"`
		} `json:"links"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return "", fmt.Errorf("decode webfinger: %w", err)
	}
	for _, l := range doc.Links {
		if l.Rel == "self" && strings.Contains(l.Type, "activity+json") && l.Href != "" {
			return l.Href, nil
		}
	}
	return "", fmt.Errorf("no actor link for %s", handle)
}

// Deliver signs body and POSTs it to inboxURL; status 0 on transport error.
func (c *Client) Deliver(ctx context.Context, key *rsa.PrivateKey, keyID, inboxURL string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, inboxURL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/activity+json")
	if err := httpsig.Sign(req, keyID, key, body); err != nil {
		return 0, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("deliver: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}
