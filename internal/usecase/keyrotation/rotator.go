// Package keyrotation re-encrypts every secret-at-rest under a new master key.
package keyrotation

import (
	"context"
	"fmt"

	"github.com/opencairn-org/cairn-core/internal/port"
)

// Cipher is an AEAD over the master key (auth.SecretBox).
type Cipher interface {
	Encrypt(plaintext, aad []byte) ([]byte, error)
	Decrypt(ciphertext, aad []byte) ([]byte, error)
}

// Status is the outcome for one ciphertext.
type Status string

const (
	Empty   Status = "empty"   // null ciphertext, nothing to do
	Rotated Status = "rotated" // re-encrypted (or, in a dry run, would be)
	Already Status = "already" // decrypts under the new key: a resumed run
	Failed  Status = "fail"    // decrypts under neither key
)

// Counts tallies the outcomes.
type Counts struct {
	Rotated, Already, Empty, Failed int
}

// Rotator decrypts with Old and encrypts with New, preserving each column's AAD.
type Rotator struct {
	Repo   port.SecretRotationRepo
	Old    Cipher
	New    Cipher
	DryRun bool
	// OnUnreadable is told about each ciphertext neither key opens; nil ignores.
	OnUnreadable func(column, id string)

	counts Counts
}

// Run walks every table; a step error stops the run, unreadable secrets do not.
func (r *Rotator) Run(ctx context.Context) (Counts, error) {
	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"external_accounts (oauth tokens)", r.oauthTokens},
		{"user_provider_configs (client secrets)", r.connectionSecrets},
		{"instance_signing_keys (account seed)", r.signingKeys},
	}
	for _, s := range steps {
		if err := s.fn(ctx); err != nil {
			return r.counts, fmt.Errorf("%s: %w", s.name, err)
		}
	}
	return r.counts, nil
}

// reencrypt returns the new-key ciphertext when the status is Rotated (nil in a dry run).
func (r *Rotator) reencrypt(ct, aad []byte) ([]byte, Status) {
	if len(ct) == 0 {
		return nil, Empty
	}
	pt, err := r.Old.Decrypt(ct, aad)
	if err != nil {
		if _, e2 := r.New.Decrypt(ct, aad); e2 == nil {
			return nil, Already
		}
		return nil, Failed
	}
	if r.DryRun {
		return nil, Rotated
	}
	out, err := r.New.Encrypt(pt, aad)
	if err != nil {
		return nil, Failed
	}
	return out, Rotated
}

func (r *Rotator) tally(s Status, column, id string) {
	switch s {
	case Rotated:
		r.counts.Rotated++
	case Already:
		r.counts.Already++
	case Empty:
		r.counts.Empty++
	case Failed:
		r.counts.Failed++
		if r.OnUnreadable != nil {
			r.OnUnreadable(column, id)
		}
	}
}

// pick keeps the original ciphertext unless it was rotated.
func pick(orig, rotated []byte, s Status) []byte {
	if s == Rotated {
		return rotated
	}
	return orig
}

func (r *Rotator) oauthTokens(ctx context.Context) error {
	rows, err := r.Repo.OAuthTokens(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		id := row.ID.String()
		aad := []byte("oauth_token:" + id)
		access, sA := r.reencrypt(row.Access, aad)
		refresh, sR := r.reencrypt(row.Refresh, aad)
		r.tally(sA, "external_accounts.access", id)
		r.tally(sR, "external_accounts.refresh", id)
		if (sA != Rotated && sR != Rotated) || r.DryRun {
			continue
		}
		if err := r.Repo.SetOAuthTokens(ctx, row.ID, pick(row.Access, access, sA), pick(row.Refresh, refresh, sR)); err != nil {
			return err
		}
	}
	return nil
}

func (r *Rotator) connectionSecrets(ctx context.Context) error {
	rows, err := r.Repo.ConnectionSecrets(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		id := row.ID.String()
		ct, s := r.reencrypt(row.Secret, []byte("user_provider_config:"+id))
		r.tally(s, "user_provider_configs", id)
		if s != Rotated || r.DryRun {
			continue
		}
		if err := r.Repo.SetConnectionSecret(ctx, row.ID, ct); err != nil {
			return err
		}
	}
	return nil
}

func (r *Rotator) signingKeys(ctx context.Context) error {
	rows, err := r.Repo.SigningKeySeeds(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		ct, s := r.reencrypt(row.Seed, []byte("signing_key:"+row.Purpose))
		r.tally(s, "instance_signing_keys", row.ID.String())
		if s != Rotated || r.DryRun {
			continue
		}
		if err := r.Repo.SetSigningKeySeed(ctx, row.ID, ct); err != nil {
			return err
		}
	}
	return nil
}
