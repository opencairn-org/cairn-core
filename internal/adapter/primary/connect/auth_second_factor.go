package connect

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
)

// defaultBearerSessionTTL applies when package main did not set SessionTTL.
const defaultBearerSessionTTL = 30 * 24 * time.Hour

// Complete2FA turns a satisfied second-factor challenge into a bearer
// session (`cairn_ses_…`) for non-browser clients.
func (s *AuthServer) Complete2FA(ctx context.Context, req *v1.Complete2FARequest) (*v1.Complete2FAResponse, error) {
	if s.SecondFactor == nil || s.Sessions == nil {
		return nil, unavailable("second-factor sign-in")
	}
	if m := req.GetMethod(); m != v1.TwoFactorMethod_TWO_FACTOR_METHOD_UNSPECIFIED && m != v1.TwoFactorMethod_TWO_FACTOR_METHOD_WEBAUTHN {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "unsupported second-factor method")
	}
	userID, err := s.SecondFactor.Complete(ctx, req.GetChallengeToken())
	if err != nil {
		return nil, actionErr(err)
	}
	u, err := s.Users.GetUser(ctx, userID)
	if err != nil || !u.IsActive() {
		return nil, connectrpc.NewError(connectrpc.CodeUnauthenticated, "account is not active")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("session token: %w", err))
	}
	token := domain.SessionTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	ttl := s.SessionTTL
	if ttl <= 0 {
		ttl = defaultBearerSessionTTL
	}
	now := time.Now().UTC()
	id, err := s.Sessions.Create(ctx, domain.Session{
		UserID: userID, TokenHash: hash[:], AuthMethod: domain.SessionAuthPasswordWithWebauthn,
		UserAgentSummary: requestHeader(ctx).Get("User-Agent"), CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(ttl),
	})
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("create session: %w", err))
	}
	return &v1.Complete2FAResponse{Session: &v1.SessionResult{
		SessionId: id.String(), SessionToken: token, ExpiresAt: timestamppb.New(now.Add(ttl)), User: userToProto(u),
	}}, nil
}

// SetPasskeySecondFactor opts the caller in or out of the passkey second factor.
func (s *AuthServer) SetPasskeySecondFactor(ctx context.Context, req *v1.SetPasskeySecondFactorRequest) (*v1.SetPasskeySecondFactorResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	if s.SecondFactor == nil {
		return nil, unavailable("second-factor sign-in")
	}
	if err := s.SecondFactor.SetEnabled(ctx, userID, req.GetEnabled()); err != nil {
		return nil, actionErr(err)
	}
	u, err := s.Users.GetUser(ctx, userID)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("reload user: %w", err))
	}
	return &v1.SetPasskeySecondFactorResponse{User: userToProto(u)}, nil
}
