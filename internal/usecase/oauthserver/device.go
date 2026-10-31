package oauthserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// userCodeAttempts bounds the retry on a user-code collision.
const userCodeAttempts = 5

// DeviceGrant is a fresh device-authorization code pair (RFC 8628 §3.2).
type DeviceGrant struct {
	DeviceCode string
	// UserCode is the display form (XXXX-XXXX).
	UserCode  string
	ExpiresIn time.Duration
	Interval  time.Duration
}

// RequestDevice issues a code pair to a client allowed the device grant.
func (s *Server) RequestDevice(ctx context.Context, creds ClientCredentials, scope string) (DeviceGrant, error) {
	client, err := s.authenticate(ctx, creds)
	if err != nil {
		return DeviceGrant{}, err
	}
	if !client.AllowsGrant(domain.GrantTypeDeviceCode) {
		return DeviceGrant{}, protoErr("unauthorized_client", "client may not use the device_code grant")
	}
	scopes := domain.FilterRequestedScopes(domain.ParseScopes(scope), client.Scopes)
	now := s.now()
	deviceCode, hash := newToken(domain.OAuthDeviceCodePrefix)
	var userCode string
	for attempt := 0; ; attempt++ {
		userCode, err = domain.NewUserCode()
		if err != nil {
			break
		}
		err = s.Repo.CreateDeviceCode(ctx, domain.OAuthDeviceCode{
			ClientID: client.ClientID, UserCode: userCode, Scope: domain.JoinScopes(scopes),
			ExpiresAt: now.Add(s.Config.DeviceCodeTTL),
		}, hash)
		if !errors.Is(err, domain.ErrUnique) || attempt+1 >= userCodeAttempts {
			break
		}
	}
	if err != nil {
		return DeviceGrant{}, serverErr("could not issue a device code", fmt.Errorf("create device code: %w", err))
	}
	return DeviceGrant{
		DeviceCode: deviceCode, UserCode: domain.DisplayUserCode(userCode),
		ExpiresIn: s.Config.DeviceCodeTTL, Interval: s.Config.DevicePollInterval,
	}, nil
}

// PollDevice is the device's polling half: tokens once the person approved,
// a protocol error (authorization_pending, slow_down, ...) until then.
func (s *Server) PollDevice(ctx context.Context, creds ClientCredentials, deviceCode string) (TokenGrant, error) {
	client, err := s.authenticate(ctx, creds)
	if err != nil {
		return TokenGrant{}, err
	}
	if !strings.HasPrefix(deviceCode, domain.OAuthDeviceCodePrefix) {
		return TokenGrant{}, protoErr("invalid_grant", "invalid device_code")
	}
	now := s.now()
	hash := HashToken(deviceCode)
	d, err := s.Repo.PollDeviceCode(ctx, hash, now)
	if err != nil || d.ClientID != client.ClientID {
		return TokenGrant{}, protoErr("invalid_grant", "device_code unknown, consumed or issued to another client")
	}
	if !now.Before(d.ExpiresAt) {
		return TokenGrant{}, protoErr("expired_token", "the device code expired; request a new one")
	}
	if d.LastPolledAt != nil && now.Sub(*d.LastPolledAt) < s.Config.DevicePollInterval {
		return TokenGrant{}, protoErr("slow_down", "poll no more often than the interval")
	}
	switch d.Status {
	case domain.DeviceCodePending:
		return TokenGrant{}, protoErr("authorization_pending", "the user has not decided yet")
	case domain.DeviceCodeDenied:
		return TokenGrant{}, protoErr("access_denied", "the user denied the request")
	case domain.DeviceCodeApproved:
		consumed, err := s.Repo.ConsumeDeviceCode(ctx, hash, now)
		if err != nil || consumed.UserID == nil {
			return TokenGrant{}, protoErr("invalid_grant", "device_code already used")
		}
		return s.issue(ctx, client.ClientID, *consumed.UserID, consumed.GrantedScope)
	}
	return TokenGrant{}, protoErr("invalid_grant", "unknown device code state")
}
