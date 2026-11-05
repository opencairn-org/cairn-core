package workerrpc

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/adapter/secondary/inmem"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/providerlink"
)

type fakeAccounts struct {
	port.ExternalAccountRepo
	byID map[domain.ExternalAccountID]domain.ExternalAccount
}

func (f *fakeAccounts) GetExternalAccount(_ context.Context, id domain.ExternalAccountID) (domain.ExternalAccount, error) {
	a, ok := f.byID[id]
	if !ok {
		return a, domain.ErrNotFound
	}
	return a, nil
}

type fakeConfigs struct {
	port.UserProviderConfigRepo
	byID map[domain.ConnectionID]domain.UserProviderConfig
}

func (f *fakeConfigs) GetByID(_ context.Context, id domain.ConnectionID) (domain.UserProviderConfig, error) {
	c, ok := f.byID[id]
	if !ok {
		return c, domain.ErrNotFound
	}
	return c, nil
}

type fakeTokens struct {
	port.OAuthTokenRepo
	stored  map[domain.ExternalAccountID]port.TokenState
	flagged map[domain.ExternalAccountID]string
}

func (f *fakeTokens) GetToken(_ context.Context, id domain.ExternalAccountID) (port.TokenState, error) {
	st, ok := f.stored[id]
	if !ok {
		return st, port.ErrTokenAccountNotFound
	}
	return st, nil
}

func (f *fakeTokens) StoreToken(_ context.Context, id domain.ExternalAccountID, in port.StoreTokenInput) error {
	f.stored[id] = in.State
	return nil
}

func (f *fakeTokens) MarkNeedsReauth(_ context.Context, id domain.ExternalAccountID, reason string) error {
	f.flagged[id] = reason
	return nil
}

func request(t *testing.T, bus *inmem.Bus, subject string, req any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(req)
	raw, err := bus.Request(context.Background(), subject, body, time.Second)
	if err != nil {
		t.Fatalf("request %s: %v", subject, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode reply %s: %v", raw, err)
	}
	return out
}

// Workers only ever read, write or flag accounts of the provider in the
// subject they are allowed to publish on.
func TestResponders_ProviderScope(t *testing.T) {
	ctx := context.Background()
	connID := domain.ConnectionID(uuid.New())
	strava := domain.ExternalAccount{ID: domain.ExternalAccountID(uuid.New()), Provider: "strava", ConnectionID: &connID}
	garmin := domain.ExternalAccount{ID: domain.ExternalAccountID(uuid.New()), Provider: "garmin", ConnectionID: &connID}
	accts := &fakeAccounts{byID: map[domain.ExternalAccountID]domain.ExternalAccount{strava.ID: strava, garmin.ID: garmin}}
	cfgs := &fakeConfigs{byID: map[domain.ConnectionID]domain.UserProviderConfig{connID: {ClientID: "login", ClientSecret: "secret"}}}
	toks := &fakeTokens{
		stored:  map[domain.ExternalAccountID]port.TokenState{strava.ID: {AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour)}},
		flagged: map[domain.ExternalAccountID]string{},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	wt := &providerlink.WorkerTokens{Tokens: toks, Accounts: accts, Configs: cfgs, Log: log}
	bus := inmem.New()
	if _, err := (&Tokens{Tokens: wt, Log: log}).Start(ctx, bus); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Logins{Tokens: wt, Log: log}).Start(ctx, bus); err != nil {
		t.Fatal(err)
	}

	got := request(t, bus, "cairn.tokens.strava.get", map[string]any{"account_id": strava.ID.String()})
	if got["access_token"] != "at" || got["client_id"] != "login" {
		t.Fatalf("own provider get: %v", got)
	}
	got = request(t, bus, "cairn.tokens.garmin.get", map[string]any{"account_id": strava.ID.String()})
	if got["error"] != "account_gone" || got["access_token"] != nil {
		t.Fatalf("foreign provider get must look like a missing account: %v", got)
	}

	store := map[string]any{"account_id": strava.ID.String(), "access_token": "new", "expires_at": time.Now().Add(2 * time.Hour)}
	if got = request(t, bus, "cairn.tokens.garmin.store", store); got["error"] != "account_gone" {
		t.Fatalf("foreign provider store: %v", got)
	}
	if toks.stored[strava.ID].AccessToken != "at" {
		t.Fatal("foreign provider store must not write")
	}
	if got = request(t, bus, "cairn.tokens.strava.store", store); got["ok"] != true {
		t.Fatalf("own provider store: %v", got)
	}

	if got = request(t, bus, "cairn.tokens.garmin.needs_reauth", map[string]any{"account_id": strava.ID.String()}); got["error"] != "account_gone" {
		t.Fatalf("foreign provider needs_reauth: %v", got)
	}
	if _, flagged := toks.flagged[strava.ID]; flagged {
		t.Fatal("foreign provider needs_reauth must not flag")
	}

	got = request(t, bus, "cairn.creds.garmin.get", map[string]any{"account_id": garmin.ID.String()})
	if got["client_id"] != "login" || got["client_secret"] != "secret" || got["email"] != "login" {
		t.Fatalf("own provider creds: %v", got)
	}
	got = request(t, bus, "cairn.creds.strava.get", map[string]any{"account_id": garmin.ID.String()})
	if got["error"] != "account_gone" || got["client_secret"] != nil {
		t.Fatalf("foreign provider creds must not leak the secret: %v", got)
	}
	// A credential provider that is not Garmin gets the same responder.
	polar := domain.ExternalAccount{ID: domain.ExternalAccountID(uuid.New()), Provider: "polar", ConnectionID: &connID}
	accts.byID[polar.ID] = polar
	if got = request(t, bus, "cairn.creds.polar.get", map[string]any{"account_id": polar.ID.String()}); got["client_id"] != "login" {
		t.Fatalf("polar creds: %v", got)
	}
}

func TestProviderOf(t *testing.T) {
	for subject, want := range map[string]string{
		"cairn.tokens.strava.get":    "strava",
		"cairn.creds.polar.get":      "polar",
		"cairn.tokens.get":           "",
		"cairn.tokens.Strava.get":    "",
		"cairn.tokens.strava.get.x":  "",
		"cairn.tokens.a-b_c.store":   "a-b_c",
		"cairn.tokens..needs_reauth": "",
	} {
		if got := providerOf(subject); got != want {
			t.Errorf("providerOf(%q) = %q, want %q", subject, got, want)
		}
	}
}
