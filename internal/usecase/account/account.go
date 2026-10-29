// Package account holds the self-service account use cases behind both the
// REST /api and the Connect AuthService: personal access tokens, passwords,
// sessions, linked OIDC identities and the editable profile. Transports only
// translate input and map port.ActionError to their own status codes.
package account

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// PasswordHasher is the subset of auth.PasswordHasher these use cases need.
type PasswordHasher interface {
	Hash(password string) (string, error)
	Verify(password, encoded string) (bool, error)
}

func invalid(err error) error { return port.NewActionError(port.ActionErrorInvalid, err.Error()) }

func internalErr(op string, err error) error {
	return port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("%s: %v", op, err))
}

// ---------------------------------------------------------------------------
// Personal access tokens
// ---------------------------------------------------------------------------

// Tokens mints, lists and revokes personal access tokens.
type Tokens struct {
	PATs port.PATRepo
}

// Create mints a token; the plaintext is returned once and only its hash is stored.
func (t *Tokens) Create(ctx context.Context, userID domain.UserID, name string, scopes []string, expiresAt *time.Time) (domain.PersonalAccessToken, string, error) {
	name, err := domain.ValidatePATName(name)
	if err != nil {
		return domain.PersonalAccessToken{}, "", invalid(err)
	}
	if expiresAt != nil && !expiresAt.After(time.Now()) {
		return domain.PersonalAccessToken{}, "", invalid(errors.New("expiry must be in the future"))
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return domain.PersonalAccessToken{}, "", internalErr("token generation", err)
	}
	token := domain.PATTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	pat := domain.PersonalAccessToken{
		UserID: userID, Name: name, Scopes: scopes,
		TokenPrefix: domain.PATDisplayPrefix(token), ExpiresAt: expiresAt,
		CreatedAt: time.Now().UTC(),
	}
	id, err := t.PATs.Create(ctx, pat, hash[:])
	if err != nil {
		return domain.PersonalAccessToken{}, "", internalErr("create token", err)
	}
	pat.ID = id
	return pat, token, nil
}

// List returns the user's tokens, newest first.
func (t *Tokens) List(ctx context.Context, userID domain.UserID) ([]domain.PersonalAccessToken, error) {
	list, err := t.PATs.ListForUser(ctx, userID)
	if err != nil {
		return nil, internalErr("list tokens", err)
	}
	return list, nil
}

// Revoke marks one of the caller's tokens revoked.
func (t *Tokens) Revoke(ctx context.Context, userID domain.UserID, id domain.PATID) error {
	if err := t.PATs.Revoke(ctx, userID, id); err != nil {
		return internalErr("revoke token", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// Sessions lists and revokes the caller's cookie sessions.
type Sessions struct {
	Repo port.SessionRepo
}

// List returns the active sessions, most recently seen first.
func (s *Sessions) List(ctx context.Context, userID domain.UserID) ([]domain.Session, error) {
	list, err := s.Repo.ListForUser(ctx, userID)
	if err != nil {
		return nil, internalErr("list sessions", err)
	}
	return list, nil
}

// Revoke ends one of the caller's sessions; another user's id reads as not found.
func (s *Sessions) Revoke(ctx context.Context, userID domain.UserID, id domain.SessionID) error {
	list, err := s.Repo.ListForUser(ctx, userID)
	if err != nil {
		return internalErr("list sessions", err)
	}
	for _, sess := range list {
		if sess.ID == id {
			if err := s.Repo.Revoke(ctx, id); err != nil {
				return internalErr("revoke session", err)
			}
			return nil
		}
	}
	return port.NewActionError(port.ActionErrorNotFound, "session not found")
}

// revokeOthers ends every active session of the user except keep.
func revokeOthers(ctx context.Context, repo port.SessionRepo, userID domain.UserID, keep domain.SessionID) error {
	list, err := repo.ListForUser(ctx, userID)
	if err != nil {
		return err
	}
	for _, sess := range list {
		if sess.ID == keep {
			continue
		}
		if err := repo.Revoke(ctx, sess.ID); err != nil {
			return err
		}
	}
	return nil
}
