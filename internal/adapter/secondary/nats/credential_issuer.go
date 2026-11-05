package nats

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"

	"github.com/opencairn-org/cairn-core/internal/auth"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// CredentialIssuer implements port.NATSCredentialIssuer.
//
// Holds the cairn-server's account signing key (NKey) decrypted in memory.
// Every IssueWorkerCredential call mints a fresh User JWT signed with
// this account key, scoping the worker's connection to the subject
// permissions encoded in the JWT.
//
// This struct is the SINGLE place in the codebase that holds plaintext
// NATS seeds. The SecretBox handles encryption-at-rest; the in-process
// copy here exists only for sign-time. Operators can rotate the key by
// calling Rotate() (atomic — DeactivateAll + CreateKey in one Tx via
// the SigningKeyRepo's caller).
type CredentialIssuer struct {
	keys    port.SigningKeyRepo
	secrets *auth.SecretBox
	// workerAccount is the nats.conf account issued users are placed in.
	workerAccount string

	// Cache of the active account NKey. Refreshed on rotation by the
	// caller; here we lazy-load on first use.
	mu         sync.Mutex
	accountKP  nkeys.KeyPair
	accountPub string
}

// NewCredentialIssuer wires the issuer. The active key is NOT loaded
// here — the first Issue or AccountPublicKey call triggers the lazy
// load so startup doesn't fail when no key exists yet (the Bootstrap
// admin endpoint creates one).
func NewCredentialIssuer(
	keys port.SigningKeyRepo,
	secrets *auth.SecretBox,
	workerAccount string,
) *CredentialIssuer {
	return &CredentialIssuer{keys: keys, secrets: secrets, workerAccount: workerAccount}
}

// IssueWorkerCredential mints a User JWT with the requested permissions.
// Process:
//
//  1. Load (and cache) the active account NKey from SigningKeyRepo.
//  2. Build jwt.UserClaims with sub = worker's user NKey pub.
//  3. Populate permissions from the input.
//  4. Set the exp claim to now+TTL.
//  5. Encode + sign with the account NKey's seed.
//
// The worker JWT's `iss` claim is the account public key (nats.conf
// auth_callout.issuer), `aud` the account the worker is placed in, and
// `sub` the user NKey nats-server generated for the connection.
func (i *CredentialIssuer) IssueWorkerCredential(
	ctx context.Context,
	in port.IssueCredentialInput,
) (port.IssueCredentialOutput, error) {
	if err := validateUserNKey(in.UserNKeyPublic); err != nil {
		return port.IssueCredentialOutput{}, err
	}
	// NATS reads an empty allow list as "allow everything".
	if len(in.Permissions.AllowPublish) == 0 || len(in.Permissions.AllowSubscribe) == 0 {
		return port.IssueCredentialOutput{}, &port.IssueValidationError{
			Field:   "Permissions",
			Message: "publish and subscribe allow lists must not be empty",
		}
	}
	if i.workerAccount == "" {
		return port.IssueCredentialOutput{}, errors.New("nats credential issuer: no worker account configured")
	}
	if in.TTL <= 0 {
		in.TTL = 24 * time.Hour
	}

	kp, accountPub, err := i.loadActiveAccountKey(ctx)
	if err != nil {
		return port.IssueCredentialOutput{}, err
	}

	claims := jwt.NewUserClaims(in.UserNKeyPublic)
	claims.IssuedAt = time.Now().UTC().Unix()
	claims.Expires = time.Now().UTC().Add(in.TTL).Unix()
	claims.Name = workerJWTName(in.Permissions)
	// Config-mode auth-callout: aud names the target account; issuer_account is refused.
	claims.Audience = i.workerAccount

	// Permissions: pub/sub allow-lists. NATS denies anything not in the
	// allow-list when these are set.
	claims.Pub.Allow = append([]string(nil), in.Permissions.AllowPublish...)
	claims.Sub.Allow = append([]string(nil), in.Permissions.AllowSubscribe...)

	if in.Permissions.AllowResponseMaxMsgs > 0 || in.Permissions.AllowResponseTTL > 0 {
		claims.Resp = &jwt.ResponsePermission{
			MaxMsgs: in.Permissions.AllowResponseMaxMsgs,
			Expires: in.Permissions.AllowResponseTTL,
		}
	}

	if in.Bearer {
		claims.BearerToken = true
	}

	encoded, err := claims.Encode(kp)
	if err != nil {
		return port.IssueCredentialOutput{}, fmt.Errorf("encode user jwt: %w", err)
	}

	return port.IssueCredentialOutput{
		UserJWT:          encoded,
		ExpiresAt:        time.Unix(claims.Expires, 0).UTC(),
		AccountPublicKey: accountPub,
	}, nil
}

// AccountPublicKey returns the cached account public key. Loads from the
// repo on first call.
func (i *CredentialIssuer) AccountPublicKey(ctx context.Context) (string, error) {
	_, pub, err := i.loadActiveAccountKey(ctx)
	if err != nil {
		return "", err
	}
	return pub, nil
}

// loadActiveAccountKey loads (and caches) the active account signing
// key. The seed is decrypted via SecretBox; nkeys.FromSeed reconstructs
// the keypair.
func (i *CredentialIssuer) loadActiveAccountKey(ctx context.Context) (nkeys.KeyPair, string, error) {
	i.mu.Lock()
	if i.accountKP != nil {
		kp, pub := i.accountKP, i.accountPub
		i.mu.Unlock()
		return kp, pub, nil
	}
	i.mu.Unlock()

	sk, err := i.keys.GetActive(ctx, domain.SigningKeyPurposeNATSAccount)
	if err != nil {
		return nil, "", fmt.Errorf("load active account signing key: %w", err)
	}

	seedPlain, err := i.secrets.Decrypt(sk.SeedEncrypted, []byte("signing_key:"+sk.Purpose))
	if err != nil {
		return nil, "", fmt.Errorf("decrypt account seed: %w", err)
	}

	kp, err := nkeys.FromSeed(seedPlain)
	if err != nil {
		return nil, "", fmt.Errorf("nkey from seed: %w", err)
	}

	pub, err := kp.PublicKey()
	if err != nil {
		return nil, "", fmt.Errorf("account public key: %w", err)
	}
	if pub != sk.PublicKey {
		return nil, "", fmt.Errorf("account key/seed mismatch: db=%s derived=%s",
			truncate(sk.PublicKey, 12), truncate(pub, 12))
	}

	i.mu.Lock()
	i.accountKP = kp
	i.accountPub = pub
	i.mu.Unlock()

	return kp, pub, nil
}

// InvalidateCache forces the next operation to reload the account key
// from the repo. Called by the rotation use case after the new key is
// committed.
func (i *CredentialIssuer) InvalidateCache() {
	i.mu.Lock()
	i.accountKP = nil
	i.accountPub = ""
	i.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// validateUserNKey checks the public-form Ed25519 NATS user key. NATS
// user keys start with 'U' and are 56 chars (RFC 4648 base32, no padding).
// Cheap to validate without a round-trip through nkeys.FromPublicKey.
func validateUserNKey(pub string) error {
	if len(pub) != 56 || pub[0] != 'U' {
		return &port.IssueValidationError{
			Field:   "UserNKeyPublic",
			Message: "must be 56-char base32 starting with 'U'",
		}
	}
	if _, err := nkeys.FromPublicKey(pub); err != nil {
		return &port.IssueValidationError{
			Field:   "UserNKeyPublic",
			Message: "invalid NATS user public key: " + err.Error(),
		}
	}
	return nil
}

// workerJWTName is the human-readable JWT name cairn-worker-<name>-<provider>
// (the worker key), read from the template's first result subject
// cairn.results.<job>.<provider>.<name>.
func workerJWTName(p domain.WorkerPermissionTemplate) string {
	if len(p.AllowPublish) == 0 {
		return "cairn-worker"
	}
	_, provider, pool, ok := domain.ResultSubjectPool(p.AllowPublish[0])
	if !ok || pool == "" {
		return "cairn-worker"
	}
	return "cairn-worker-" + domain.WorkerKey(pool, provider)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// Account-key bootstrap
// ---------------------------------------------------------------------------

// BootstrapAccountKey generates a fresh NATS account NKey and persists
// it (encrypted) into instance_signing_keys. Returns the public key so
// the operator can update the NATS server config.
//
// Caller (typically an admin endpoint) must ensure no active key exists
// already — running this twice would create two active keys for the
// same purpose, violating the unique constraint. The use case wraps
// this in DeactivateAll+CreateKey when an existing key is being rotated.
func BootstrapAccountKey(
	ctx context.Context,
	keys port.SigningKeyRepo,
	secrets *auth.SecretBox,
	createdBy *domain.UserID,
) (domain.SigningKey, error) {
	if existing, err := keys.GetActive(ctx, domain.SigningKeyPurposeNATSAccount); err == nil {
		return existing, errors.New("active signing key already exists; use Rotate to replace")
	}

	kp, err := nkeys.CreateAccount()
	if err != nil {
		return domain.SigningKey{}, fmt.Errorf("create account key: %w", err)
	}
	pub, err := kp.PublicKey()
	if err != nil {
		return domain.SigningKey{}, fmt.Errorf("get account public key: %w", err)
	}
	seed, err := kp.Seed()
	if err != nil {
		return domain.SigningKey{}, fmt.Errorf("get account seed: %w", err)
	}
	defer wipe(seed)

	encrypted, err := secrets.Encrypt(seed, []byte("signing_key:"+domain.SigningKeyPurposeNATSAccount))
	if err != nil {
		return domain.SigningKey{}, fmt.Errorf("encrypt account seed: %w", err)
	}

	created, err := keys.CreateKey(ctx, domain.SigningKey{
		Purpose:         domain.SigningKeyPurposeNATSAccount,
		PublicKey:       pub,
		SeedEncrypted:   encrypted,
		Active:          true,
		CreatedAt:       time.Now().UTC(),
		CreatedByUserID: createdBy,
	})
	if err != nil {
		return domain.SigningKey{}, fmt.Errorf("persist account signing key: %w", err)
	}
	return created, nil
}

// RotateAccountKey generates a NEW account signing key, deactivates the old
// one, and inserts the new as active — returning both the new public key (to
// add to nats-server.conf) and the old one (keep it trusted during the overlap
// window so workers holding JWTs signed by the old key stay connected until
// they renew, then drop it). Run inside a transaction so the deactivate +
// insert are atomic vs the one-active-per-purpose unique index — the caller
// wraps this in TxManager.InTx. After it returns, invalidate the live
// CredentialIssuer's cache so new JWTs are signed with the new key.
func RotateAccountKey(
	ctx context.Context,
	keys port.SigningKeyRepo,
	secrets *auth.SecretBox,
	createdBy *domain.UserID,
) (newKey domain.SigningKey, oldPublicKey string, err error) {
	if old, e := keys.GetActive(ctx, domain.SigningKeyPurposeNATSAccount); e == nil {
		oldPublicKey = old.PublicKey
	}

	kp, err := nkeys.CreateAccount()
	if err != nil {
		return domain.SigningKey{}, "", fmt.Errorf("create account key: %w", err)
	}
	pub, err := kp.PublicKey()
	if err != nil {
		return domain.SigningKey{}, "", fmt.Errorf("get account public key: %w", err)
	}
	seed, err := kp.Seed()
	if err != nil {
		return domain.SigningKey{}, "", fmt.Errorf("get account seed: %w", err)
	}
	defer wipe(seed)

	encrypted, err := secrets.Encrypt(seed, []byte("signing_key:"+domain.SigningKeyPurposeNATSAccount))
	if err != nil {
		return domain.SigningKey{}, "", fmt.Errorf("encrypt account seed: %w", err)
	}

	// Deactivate the old key first to satisfy the partial unique index, then
	// insert the new active key (atomic within the caller's transaction).
	if err := keys.DeactivateAll(ctx, domain.SigningKeyPurposeNATSAccount, time.Now().UTC()); err != nil {
		return domain.SigningKey{}, "", fmt.Errorf("deactivate old signing key: %w", err)
	}
	created, err := keys.CreateKey(ctx, domain.SigningKey{
		Purpose:         domain.SigningKeyPurposeNATSAccount,
		PublicKey:       pub,
		SeedEncrypted:   encrypted,
		Active:          true,
		CreatedAt:       time.Now().UTC(),
		CreatedByUserID: createdBy,
	})
	if err != nil {
		return domain.SigningKey{}, "", fmt.Errorf("persist new account signing key: %w", err)
	}
	return created, oldPublicKey, nil
}

// wipe zeroes a byte slice. Used on plaintext seeds after we no longer
// need them. Not a strong defense (Go runtime may have copies elsewhere)
// but reduces the lifetime of plaintext key material.
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
