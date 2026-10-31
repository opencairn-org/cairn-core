package account

import (
	"context"
	"fmt"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Passkeys stores and manages WebAuthn credentials; the ceremony itself
// runs in the transport, the credential blob is opaque here.
type Passkeys struct {
	Repo  port.PasskeyRepo
	Users port.UserReader
}

// Holder loads a user and their passkeys for a WebAuthn ceremony.
func (p *Passkeys) Holder(ctx context.Context, userID domain.UserID) (domain.User, []domain.Passkey, error) {
	u, err := p.Users.GetUser(ctx, userID)
	if err != nil {
		return domain.User{}, nil, err
	}
	keys, err := p.Repo.ListByUser(ctx, userID)
	if err != nil {
		return domain.User{}, nil, err
	}
	return u, keys, nil
}

// Register stores a freshly attested credential.
func (p *Passkeys) Register(ctx context.Context, userID domain.UserID, credentialID, credentialJSON []byte, name string) error {
	if _, err := p.Repo.Create(ctx, domain.Passkey{
		UserID: userID, CredentialID: credentialID, CredentialJSON: credentialJSON, Name: name,
	}); err != nil {
		return fmt.Errorf("store passkey: %w", err)
	}
	return nil
}

// Used records an assertion's advanced credential and returns its passkey.
func (p *Passkeys) Used(ctx context.Context, credentialID, credentialJSON []byte) (domain.Passkey, error) {
	_ = p.Repo.UpdateCredential(ctx, credentialID, credentialJSON) // the sign counter is advisory
	return p.Repo.GetByCredentialID(ctx, credentialID)
}

// List returns the caller's passkeys and whether they gate password sign-in.
func (p *Passkeys) List(ctx context.Context, userID domain.UserID) ([]domain.Passkey, bool, error) {
	keys, err := p.Repo.ListByUser(ctx, userID)
	if err != nil {
		return nil, false, err
	}
	u, _ := p.Users.GetUser(ctx, userID)
	return keys, u.PasskeySecondFactor, nil
}

// Rename relabels one of the caller's passkeys.
func (p *Passkeys) Rename(ctx context.Context, userID domain.UserID, id domain.PasskeyID, name string) error {
	return p.Repo.Rename(ctx, userID, id, name)
}

// Delete removes one of the caller's passkeys.
func (p *Passkeys) Delete(ctx context.Context, userID domain.UserID, id domain.PasskeyID) error {
	return p.Repo.Delete(ctx, userID, id)
}
