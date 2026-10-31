package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/httpsig"
)

// federation_interop_test exercises the cross-instance ActivityPub wire protocol
// end to end over real HTTP — the parts most likely to break interop with
// another Cairn (or a Mastodon-family server): a remote actor publishes its
// public key, a signed activity crosses the boundary, and the receiving side
// runs the exact inbound auth path (keyId → fetch actor → parse key → verify
// signature) before parsing the object into a feed item.
//
// It needs no database: it drives the pure protocol code (httpsig, the actor
// doc shape, the Create/Delete object format) against an httptest "remote
// instance". The full DB-backed handler round-trip is proven separately by the
// two-instance harness (scripts/federation-interop.sh).

func pubKeyPEM(t *testing.T, k *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// remoteInstance is a minimal stand-in for a peer server: it serves one actor
// document (with its RSA public key) at /users/<name>, the way Cairn's own
// actor endpoint does.
func remoteInstance(t *testing.T, name string, key *rsa.PrivateKey) (*httptest.Server, string) {
	t.Helper()
	var actorURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/"+name {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/activity+json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"@context":          []any{"https://www.w3.org/ns/activitystreams", "https://w3id.org/security/v1"},
			"type":              "Person",
			"id":                actorURL,
			"preferredUsername": name,
			"inbox":             actorURL + "/inbox",
			"outbox":            actorURL + "/outbox",
			"publicKey": map[string]any{
				"id":           actorURL + "#main-key",
				"owner":        actorURL,
				"publicKeyPem": pubKeyPEM(t, key),
			},
		})
	}))
	actorURL = srv.URL + "/users/" + name
	return srv, actorURL
}

// TestFederationInterop_InboundActivityAcrossHTTP simulates a remote actor (Bob)
// signing an activity Create and delivering it to a local inbox, then performs
// the receiving side's auth + parse exactly as cmd/server's inbox handler does.
func TestFederationInterop_InboundActivityAcrossHTTP(t *testing.T) {
	bobKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv, bobActor := remoteInstance(t, "bob", bobKey)
	defer srv.Close()

	// Bob builds + signs a Create exactly the way Cairn's publisher does, then
	// POSTs it to Alice's inbox.
	act := domain.Activity{
		ID:             domain.ActivityID(uuid.New()),
		UserID:         domain.UserID(uuid.New()),
		Type:           domain.ActivityTypeRun,
		Title:          "Morning Run",
		Description:    "felt good",
		StartTime:      time.Now().UTC().Add(-time.Hour),
		MovingDuration: 30 * time.Minute,
		Privacy:        domain.PrivacyPublic,
	}
	objID := bobActor + "/activities/" + act.ID.String()
	create := buildActivityCreate(srv.URL, bobActor, objID, act, allCats)
	body, err := json.Marshal(create)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://alice.test/users/alice/inbox", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/activity+json")
	if err := httpsig.Sign(req, bobActor+"#main-key", bobKey, body); err != nil {
		t.Fatalf("sign: %v", err)
	}

	// --- receiving side: the inbox's authentication steps ---
	keyID, ok := httpsig.KeyIDFromRequest(req)
	if !ok {
		t.Fatal("no keyId on signed request")
	}
	signerActor := strings.SplitN(keyID, "#", 2)[0]
	if signerActor != bobActor {
		t.Fatalf("signer actor = %q, want %q", signerActor, bobActor)
	}

	// Dereference the signer's actor document over real HTTP and parse its key.
	resp, err := http.Get(signerActor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var actor apActor
	if err := json.NewDecoder(resp.Body).Decode(&actor); err != nil {
		t.Fatalf("decode actor: %v", err)
	}
	if actor.Inbox == "" || actor.PublicKey.PublicKeyPem == "" {
		t.Fatal("fetched actor missing inbox or public key")
	}
	pub, err := parsePublicKeyPEM(actor.PublicKey.PublicKeyPem)
	if err != nil {
		t.Fatalf("parse key: %v", err)
	}

	// The crux: the signature Bob produced verifies against the key we fetched.
	if err := httpsig.Verify(req, body, pub); err != nil {
		t.Fatalf("interop signature verification failed: %v", err)
	}

	// And the object parses into the feed item the receiver would store.
	var env apActivity
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Type != "Create" {
		t.Fatalf("activity type = %q", env.Type)
	}
	item, ok := feedItemFromObject(domain.UserID(uuid.New()), bobActor, env.Object)
	if !ok {
		t.Fatal("receiver could not parse the Create object into a feed item")
	}
	if item.Name != "Morning Run" || item.Sport != "run" {
		t.Errorf("feed item = name:%q sport:%q", item.Name, item.Sport)
	}

	// The matching Delete targets the same object id, so the receiver's Delete
	// handler removes exactly this item.
	del := buildActivityDelete(bobActor, objID)
	if del["object"] != item.ActivityAPID {
		t.Errorf("Delete object %v != feed item ap id %q", del["object"], item.ActivityAPID)
	}

	// Negative cases the inbox MUST reject.
	if err := httpsig.Verify(req, append(body, '!'), pub); err == nil {
		t.Error("a tampered body must fail verification")
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	if err := httpsig.Verify(req, body, &other.PublicKey); err == nil {
		t.Error("a different key must fail verification")
	}
}

// TestFederationInterop_OutboundSignatureIsVerifiable proves the reverse
// direction: a request Cairn signs is verifiable by an independent peer that
// fetched Cairn's published key — i.e. our Sign output interoperates.
func TestFederationInterop_OutboundSignatureIsVerifiable(t *testing.T) {
	aliceKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	// Alice's instance publishes her actor + key.
	srv, aliceActor := remoteInstance(t, "alice", aliceKey)
	defer srv.Close()

	follow := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       aliceActor + "#follows/1",
		"type":     "Follow",
		"actor":    aliceActor,
		"object":   "https://peer.test/users/bob",
	}
	body, _ := json.Marshal(follow)
	req, _ := http.NewRequest(http.MethodPost, "https://peer.test/users/bob/inbox", bytes.NewReader(body))
	if err := httpsig.Sign(req, aliceActor+"#main-key", aliceKey, body); err != nil {
		t.Fatal(err)
	}

	// The peer fetches Alice's key and verifies — the interop guarantee.
	resp, err := http.Get(aliceActor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var actor apActor
	if err := json.NewDecoder(resp.Body).Decode(&actor); err != nil {
		t.Fatal(err)
	}
	pub, err := parsePublicKeyPEM(actor.PublicKey.PublicKeyPem)
	if err != nil {
		t.Fatal(err)
	}
	if err := httpsig.Verify(req, body, pub); err != nil {
		t.Fatalf("peer could not verify Cairn's signed request: %v", err)
	}
}
