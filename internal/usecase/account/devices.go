package account

import (
	"context"
	"errors"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ---------------------------------------------------------------------------
// Device authorization (RFC 8628): the approving side
// ---------------------------------------------------------------------------

// DeviceAuthorizations looks pending device sign-ins up by their user code and
// records the user's decision. Both the browser page and the Connect RPCs run
// on it.
type DeviceAuthorizations struct {
	OAuth port.OAuthServerRepo
	Now   func() time.Time
}

// PendingDevice is what the approving user sees before deciding.
type PendingDevice struct {
	Client domain.OAuthClient
	Code   domain.OAuthDeviceCode
	// Scopes is what approval would grant: the request, capped to what the
	// approving credential may hand out.
	Scopes []string
}

func (d *DeviceAuthorizations) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}

// Lookup resolves a typed user code to its pending request. approverScopes is
// nil for an unrestricted credential (cookie session, PAT); an OAuth caller
// passes its own scopes and sees the capped grant.
func (d *DeviceAuthorizations) Lookup(ctx context.Context, userCode string, approverScopes []string) (PendingDevice, error) {
	code, err := domain.NormalizeUserCode(userCode)
	if err != nil {
		return PendingDevice{}, invalid(err)
	}
	pending, err := d.OAuth.FindDeviceCodeByUserCode(ctx, code, d.now())
	if errors.Is(err, domain.ErrNotFound) {
		return PendingDevice{}, port.NewActionError(port.ActionErrorNotFound, "no device is waiting for this code; it may have expired")
	}
	if err != nil {
		return PendingDevice{}, internalErr("find device code", err)
	}
	if pending.Status != domain.DeviceCodePending {
		return PendingDevice{}, port.NewActionError(port.ActionErrorNotFound, "this code has already been decided")
	}
	client, err := d.OAuth.GetClient(ctx, pending.ClientID)
	if err != nil {
		return PendingDevice{}, internalErr("device client", err)
	}
	requested := domain.FilterRequestedScopes(domain.ParseScopes(pending.Scope), client.Scopes)
	return PendingDevice{Client: client, Code: pending, Scopes: domain.CapScopes(requested, approverScopes)}, nil
}

// Decide approves or denies the request behind userCode as userID and returns
// the granted scopes (empty on deny).
func (d *DeviceAuthorizations) Decide(ctx context.Context, userID domain.UserID, userCode string, approve bool, approverScopes []string) ([]string, error) {
	pending, err := d.Lookup(ctx, userCode, approverScopes)
	if err != nil {
		return nil, err
	}
	granted := []string{}
	if approve {
		granted = pending.Scopes
	}
	err = d.OAuth.DecideDeviceCode(ctx, pending.Code.UserCode, userID, approve, domain.JoinScopes(granted), d.now())
	if errors.Is(err, domain.ErrNotFound) {
		return nil, port.NewActionError(port.ActionErrorNotFound, "this code has already been decided or expired")
	}
	if err != nil {
		return nil, internalErr("decide device code", err)
	}
	return granted, nil
}
