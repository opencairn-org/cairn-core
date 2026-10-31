package keyrotation

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// fakeCipher "encrypts" as key|aad|plaintext.
type fakeCipher struct{ key string }

func (c fakeCipher) Encrypt(pt, aad []byte) ([]byte, error) {
	return append([]byte(c.key+"|"+string(aad)+"|"), pt...), nil
}

func (c fakeCipher) Decrypt(ct, aad []byte) ([]byte, error) {
	prefix := []byte(c.key + "|" + string(aad) + "|")
	if !bytes.HasPrefix(ct, prefix) {
		return nil, errors.New("open failed")
	}
	return ct[len(prefix):], nil
}

type fakeRepo struct {
	port.SecretRotationRepo
	tokens  []port.OAuthTokenCiphertexts
	conns   []port.ConnectionSecretCiphertext
	keys    []port.SigningKeySeedCiphertext
	writes  int
	connErr error
}

func (f *fakeRepo) OAuthTokens(context.Context) ([]port.OAuthTokenCiphertexts, error) {
	return f.tokens, nil
}

func (f *fakeRepo) SetOAuthTokens(_ context.Context, id domain.ExternalAccountID, a, r []byte) error {
	for i := range f.tokens {
		if f.tokens[i].ID == id {
			f.tokens[i].Access, f.tokens[i].Refresh = a, r
		}
	}
	f.writes++
	return nil
}

func (f *fakeRepo) ConnectionSecrets(context.Context) ([]port.ConnectionSecretCiphertext, error) {
	return f.conns, f.connErr
}

func (f *fakeRepo) SetConnectionSecret(_ context.Context, id domain.ConnectionID, s []byte) error {
	for i := range f.conns {
		if f.conns[i].ID == id {
			f.conns[i].Secret = s
		}
	}
	f.writes++
	return nil
}

func (f *fakeRepo) SigningKeySeeds(context.Context) ([]port.SigningKeySeedCiphertext, error) {
	return f.keys, nil
}

func (f *fakeRepo) SetSigningKeySeed(_ context.Context, id domain.SigningKeyID, s []byte) error {
	for i := range f.keys {
		if f.keys[i].ID == id {
			f.keys[i].Seed = s
		}
	}
	f.writes++
	return nil
}

func seal(c fakeCipher, aad, pt string) []byte {
	ct, _ := c.Encrypt([]byte(pt), []byte(aad))
	return ct
}

func newRepo(old, other fakeCipher) *fakeRepo {
	acct, conn, key := domain.ExternalAccountID(uuid.New()), domain.ConnectionID(uuid.New()), domain.SigningKeyID(uuid.New())
	return &fakeRepo{
		tokens: []port.OAuthTokenCiphertexts{{ID: acct, Access: seal(old, "oauth_token:"+acct.String(), "a"), Refresh: nil}},
		conns:  []port.ConnectionSecretCiphertext{{ID: conn, Secret: seal(other, "user_provider_config:"+conn.String(), "s")}},
		keys:   []port.SigningKeySeedCiphertext{{ID: key, Purpose: "nats_account", Seed: seal(old, "signing_key:nats_account", "seed")}},
	}
}

func TestRotator(t *testing.T) {
	ctx := context.Background()
	old, next, junk := fakeCipher{"old"}, fakeCipher{"new"}, fakeCipher{"junk"}

	// Dry run counts but never writes.
	repo := newRepo(old, junk)
	var unreadable []string
	got, err := (&Rotator{Repo: repo, Old: old, New: old, DryRun: true, OnUnreadable: func(col, _ string) { unreadable = append(unreadable, col) }}).Run(ctx)
	if err != nil || got != (Counts{Rotated: 2, Empty: 1, Failed: 1}) || repo.writes != 0 || len(unreadable) != 1 || unreadable[0] != "user_provider_configs" {
		t.Fatalf("dry run: %+v %v writes=%d unreadable=%v", got, err, repo.writes, unreadable)
	}

	// Real run rewrites readable secrets; a rerun finds them already rotated.
	repo = newRepo(old, old)
	if got, err = (&Rotator{Repo: repo, Old: old, New: next}).Run(ctx); err != nil || got != (Counts{Rotated: 3, Empty: 1}) || repo.writes != 3 {
		t.Fatalf("rotate: %+v %v writes=%d", got, err, repo.writes)
	}
	if pt, err := next.Decrypt(repo.keys[0].Seed, []byte("signing_key:nats_account")); err != nil || string(pt) != "seed" {
		t.Fatalf("seed not re-encrypted: %q %v", pt, err)
	}
	if repo.tokens[0].Refresh != nil {
		t.Fatal("empty refresh token was rewritten")
	}
	if got, err = (&Rotator{Repo: repo, Old: old, New: next}).Run(ctx); err != nil || got != (Counts{Already: 3, Empty: 1}) {
		t.Fatalf("rerun: %+v %v", got, err)
	}

	// A load error names the step.
	repo.connErr = errors.New("boom")
	if _, err = (&Rotator{Repo: repo, Old: old, New: next}).Run(ctx); err == nil || err.Error() != "user_provider_configs (client secrets): boom" {
		t.Fatalf("step error: %v", err)
	}
}
