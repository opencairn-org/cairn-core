package federation

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Remote reaches remote actors under the instance blocklist.
type Remote struct {
	Client port.FederationClient
	Actors port.FederationActorRepo
	Blocks port.FederationBlockRepo // nil: nothing blocked
}

// Blocked reports whether host is defederated.
func (r *Remote) Blocked(ctx context.Context, host string) bool {
	if r == nil || r.Blocks == nil || host == "" {
		return false
	}
	blocked, _ := r.Blocks.IsBlocked(ctx, host)
	return blocked
}

// Actor returns a cached or freshly fetched remote actor with its parsed key.
func (r *Remote) Actor(ctx context.Context, actorID string) (domain.FederatedActor, *rsa.PublicKey, error) {
	// Never dereference an actor on a blocked domain.
	if r.Blocked(ctx, HostOf(actorID)) {
		return domain.FederatedActor{}, nil, fmt.Errorf("domain blocked: %s", HostOf(actorID))
	}
	if a, err := r.Actors.Get(ctx, actorID); err == nil {
		if pub, perr := ParsePublicKeyPEM(a.PublicKeyPEM); perr == nil {
			return a, pub, nil
		}
	}
	a, err := r.Client.FetchActor(ctx, actorID)
	if err != nil {
		return domain.FederatedActor{}, nil, err
	}
	pub, err := ParsePublicKeyPEM(a.PublicKeyPEM)
	if err != nil {
		return domain.FederatedActor{}, nil, err
	}
	_ = r.Actors.Upsert(ctx, a)
	return a, pub, nil
}

// ResolveHandle turns user@host into an actor URL.
func (r *Remote) ResolveHandle(ctx context.Context, handle string) (string, error) {
	return r.Client.ResolveHandle(ctx, handle)
}

// Sender signs bodies with a local user's key and POSTs them.
type Sender struct {
	Keys   port.FederationKeyRepo
	Client port.FederationClient
}

// DeliverBody posts a raw activity; the status (0 on transport error) lets
// the retry scheduler tell permanent from transient failures.
func (s *Sender) DeliverBody(ctx context.Context, from domain.UserID, actorID, inbox string, body []byte) (int, error) {
	priv, err := s.Keys.GetPrivateKey(ctx, from)
	if err != nil {
		return 0, fmt.Errorf("load signing key: %w", err)
	}
	return s.Client.Deliver(ctx, priv, actorID+"#main-key", inbox, body)
}

// Send delivers an activity at once, for the interactive Follow/Accept
// handshakes; Create fan-out goes through the durable queue.
func (s *Sender) Send(ctx context.Context, from domain.UserID, actorID, inbox string, activity any) error {
	body, err := json.Marshal(activity)
	if err != nil {
		return err
	}
	code, err := s.DeliverBody(ctx, from, actorID, inbox, body)
	if err != nil {
		return err
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("deliver to %s: status %d", inbox, code)
	}
	return nil
}
