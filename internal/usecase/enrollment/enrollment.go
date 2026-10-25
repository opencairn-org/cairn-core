// Package enrollment implements the worker-enrollment lifecycle: an
// admin pre-authorises a worker by creating an enrollment offer; the
// worker presents the offer's token at NATS-connect time; the cairn
// server's NATS auth-callout handler validates the token, mints a
// short-lived NATS user JWT, and admits the connection.
//
// See docs/architecture.md §4.x "Worker Enrollment & Dynamic NATS
// Credentials" for the full design rationale.
//
// This package contains the use cases. Adapters (postgres for the repo,
// nats for the credential issuer) live elsewhere.
package enrollment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ---------------------------------------------------------------------------
// CreateWorkerEnrollment
//
// Admin-side use case. Generates a high-entropy token, hashes it, stores
// the hash in worker_enrollments. The plaintext token is returned once
// in the result struct and the caller is responsible for displaying it
// to the admin and immediately forgetting it (no logging, no persistence).
// ---------------------------------------------------------------------------

type CreateWorkerEnrollment struct {
	enrollments port.WorkerEnrollmentRepo

	now      func() time.Time
	newID    func() uuid.UUID
	randRead func([]byte) (int, error)
}

func NewCreateWorkerEnrollment(
	enrollments port.WorkerEnrollmentRepo,
	now func() time.Time,
	newID func() uuid.UUID,
	randRead func([]byte) (int, error),
) *CreateWorkerEnrollment {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if newID == nil {
		newID = func() uuid.UUID {
			id, err := uuid.NewV7()
			if err != nil {
				return uuid.New()
			}
			return id
		}
	}
	if randRead == nil {
		randRead = rand.Read
	}
	return &CreateWorkerEnrollment{
		enrollments: enrollments,
		now:         now,
		newID:       newID,
		randRead:    randRead,
	}
}

type CreateEnrollmentInput struct {
	// Provider scopes the worker's NATS permissions ([a-z0-9_-]+). Required.
	Provider string
	// Name is the admin-defined NATS-safe worker name ([a-z0-9-]). With
	// Provider it forms the worker key {name}-{provider}.
	Name string
	// Version is the worker's integer schema version (part of the admission
	// identity). Defaults to 1 when <= 0. Bumping it = a new worker = a new
	// enrollment.
	Version            int
	WorkerNamePattern  string // legacy glob; "*" allowed; default "*" if empty
	ExpiresIn          time.Duration
	MaxUses            int // 0 = unlimited
	Note               string
	CreatedBy          *domain.UserID
	PermissionTemplate string // "standard" by default
}

type CreateEnrollmentResult struct {
	Enrollment domain.WorkerEnrollment

	// Token is the plaintext bootstrap token. Display ONCE to the admin
	// and never log. After the function returns, the only copy of this
	// value should be the operator's clipboard.
	Token string
}

// checkWorkerKeyUnique refuses a name whose webhook/control key
// {name}-{provider} equals another pool's ("a-b"+"c" vs "a"+"b-c").
func (uc *CreateWorkerEnrollment) checkWorkerKeyUnique(ctx context.Context, in CreateEnrollmentInput) error {
	if in.Name == "" {
		return nil
	}
	existing, err := uc.enrollments.ListEnrollments(ctx, port.ListEnrollmentsFilter{})
	if err != nil {
		return fmt.Errorf("list enrollments: %w", err)
	}
	key := domain.WorkerKey(in.Name, in.Provider)
	for _, e := range existing {
		if e.WorkerKey() == key && (e.Name != in.Name || e.Provider != in.Provider) {
			return fmt.Errorf("%w: worker key %q is already used by %s/%s", ErrInvalidInput, key, e.Provider, e.Name)
		}
	}
	return nil
}

// createDefaults fills the optional enrollment fields.
func createDefaults(in CreateEnrollmentInput) (pattern string, version int, tpl string) {
	pattern, version, tpl = in.WorkerNamePattern, in.Version, in.PermissionTemplate
	if pattern == "" {
		pattern = "*"
	}
	if version <= 0 {
		version = 1
	}
	if tpl == "" {
		tpl = "standard"
	}
	return pattern, version, tpl
}

// Execute generates a 32-byte cryptographic token, hashes it, and stores
// the enrollment row. Errors fall into three buckets:
//
//   - input validation (bad provider, zero expiry): wrapped ErrInvalidInput
//   - randomness failure: wrapped crypto/rand error (shouldn't happen)
//   - hash collision in DB: retried once, then propagated
func (uc *CreateWorkerEnrollment) Execute(
	ctx context.Context,
	in CreateEnrollmentInput,
) (CreateEnrollmentResult, error) {
	if err := validateCreateInput(in); err != nil {
		return CreateEnrollmentResult{}, err
	}
	if err := uc.checkWorkerKeyUnique(ctx, in); err != nil {
		return CreateEnrollmentResult{}, err
	}

	pattern, version, tpl := createDefaults(in)

	now := uc.now()

	// Single retry on hash collision. Real-world probability with
	// 256-bit entropy is ~0, but the contract is "regenerate then fail
	// cleanly" so callers don't have to.
	const maxAttempts = 2
	var token string
	var hash []byte
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		t, h, err := uc.generateTokenAndHash()
		if err != nil {
			return CreateEnrollmentResult{}, fmt.Errorf("generate token: %w", err)
		}
		token = t
		hash = h

		e := domain.WorkerEnrollment{
			ID:                 domain.WorkerEnrollmentID(uc.newID()),
			TokenHash:          hash,
			Name:               in.Name,
			Provider:           in.Provider,
			Version:            version,
			WorkerNamePattern:  pattern,
			PermissionTemplate: tpl,
			CreatedAt:          now,
			CreatedByUserID:    in.CreatedBy,
			Note:               in.Note,
			ExpiresAt:          now.Add(in.ExpiresIn),
			MaxUses:            in.MaxUses,
			Uses:               0,
		}

		stored, err := uc.enrollments.CreateEnrollment(ctx, e)
		if errors.Is(err, port.ErrTokenHashConflict) {
			continue
		}
		if err != nil {
			return CreateEnrollmentResult{}, fmt.Errorf("persist enrollment: %w", err)
		}
		return CreateEnrollmentResult{Enrollment: stored, Token: token}, nil
	}

	return CreateEnrollmentResult{}, fmt.Errorf("persist enrollment: %w after %d attempts", port.ErrTokenHashConflict, maxAttempts)
}

// generateTokenAndHash returns (base64-url-encoded 32 bytes, sha256(raw)).
// 32 bytes of entropy → 43 character base64url string, fits comfortably
// in an environment variable.
func (uc *CreateWorkerEnrollment) generateTokenAndHash() (string, []byte, error) {
	raw := make([]byte, 32)
	if _, err := uc.randRead(raw); err != nil {
		return "", nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	return token, sum[:], nil
}

var ErrInvalidInput = errors.New("enrollment: invalid input")

func validateCreateInput(in CreateEnrollmentInput) error {
	// The provider scopes the issued NATS permissions; without it they would be empty.
	if !domain.ValidProviderID(in.Provider) {
		return fmt.Errorf("%w: provider required — lowercase letters, digits, hyphens, underscores", ErrInvalidInput)
	}
	if !domain.ValidWorkerName(in.Name) {
		return fmt.Errorf("%w: name required — lowercase letters, digits, hyphens (no leading/trailing hyphen)", ErrInvalidInput)
	}
	if in.ExpiresIn <= 0 {
		return fmt.Errorf("%w: expires_in must be positive", ErrInvalidInput)
	}
	if in.ExpiresIn > 365*24*time.Hour {
		return fmt.Errorf("%w: expires_in capped at 365d", ErrInvalidInput)
	}
	if in.MaxUses < 0 {
		return fmt.Errorf("%w: max_uses cannot be negative (use 0 for unlimited)", ErrInvalidInput)
	}
	return nil
}

// ---------------------------------------------------------------------------
// ProcessAuthCallout
//
// Hot path. Called once per worker NATS-connect attempt by the
// subscriber on `$SYS.REQ.USER.AUTH`. Resolves an enrollment token to
// a signed user-JWT, or rejects.
//
// The handler is structured so all rejection paths return the same
// error type (AuthRejection), with a public-safe reason and an internal
// detail. The NATS adapter renders this into the NATS auth-callout
// reply format (`v2.AuthorizationResponseClaims`).
// ---------------------------------------------------------------------------

type ProcessAuthCallout struct {
	enrollments port.WorkerEnrollmentRepo
	issuer      port.NATSCredentialIssuer
	tx          port.TxManager

	now   func() time.Time
	newID func() uuid.UUID

	// Default TTL for user-JWTs. 24h balances "low revocation latency"
	// with "no constant token-refresh hammering".
	defaultJWTTTL time.Duration
}

func NewProcessAuthCallout(
	enrollments port.WorkerEnrollmentRepo,
	issuer port.NATSCredentialIssuer,
	tx port.TxManager,
	now func() time.Time,
	newID func() uuid.UUID,
	defaultJWTTTL time.Duration,
) *ProcessAuthCallout {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if newID == nil {
		newID = func() uuid.UUID {
			id, err := uuid.NewV7()
			if err != nil {
				return uuid.New()
			}
			return id
		}
	}
	if defaultJWTTTL <= 0 {
		defaultJWTTTL = 24 * time.Hour
	}
	return &ProcessAuthCallout{
		enrollments:   enrollments,
		issuer:        issuer,
		tx:            tx,
		now:           now,
		newID:         newID,
		defaultJWTTTL: defaultJWTTTL,
	}
}

// AuthCalloutInput is what the NATS auth-callout subscriber extracts
// from the inbound auth request and hands to the use case.
type AuthCalloutInput struct {
	// EnrollmentToken is the plaintext token the worker passed in
	// CONNECT-options.token. The use case hashes it and looks up the
	// enrollment row.
	EnrollmentToken string

	// UserNKeyPublic is the per-connection user nkey nats-server assigns in the auth request.
	UserNKeyPublic string

	// WorkerName from the CONNECT client_name option. Must match the
	// enrollment's WorkerNamePattern.
	WorkerName string

	// WorkerInstanceID from the CONNECT options. Used in the grant
	// audit row. Falls back to a random id if empty.
	WorkerInstanceID string

	// WorkerVersion reported by the worker. Empty if not supplied.
	WorkerVersion string

	// ClientHost is the textual client IP as observed by NATS. Empty if
	// NATS didn't include it in the auth request.
	ClientHost string
}

// AuthCalloutResult is the success-case output: the JWT to admit, plus
// metadata for logging.
type AuthCalloutResult struct {
	UserJWT          string
	GrantID          domain.WorkerCredentialGrantID
	EnrollmentID     domain.WorkerEnrollmentID
	ExpiresAt        time.Time
	AccountPublicKey string
}

// AuthRejection is the failure type. Reason is safe to expose to the
// worker (NATS' auth-callout reply error field); Detail is for logs.
type AuthRejection struct {
	Reason string
	Detail string
	Cause  error
	// EnrollmentID is set once the token resolved to an enrollment.
	EnrollmentID *domain.WorkerEnrollmentID
}

func (e *AuthRejection) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("auth rejected: %s (%s): %v", e.Reason, e.Detail, e.Cause)
	}
	return fmt.Sprintf("auth rejected: %s (%s)", e.Reason, e.Detail)
}

func (e *AuthRejection) Unwrap() error { return e.Cause }

// Execute is the hot path.
//
// Steps:
//  1. Hash the token; lookup enrollment by hash.
//  2. Validate enrollment (revoked/expired/exhausted, then provider set) —
//     constant-time hash compare prevents timing oracle on token validity.
//  3. Validate worker-name against enrollment pattern.
//  4. Validate user-nkey format.
//  5. Mint JWT (issuer call; signs in-process).
//  6. Atomically: increment enrollment.uses + create credential-grant row.
//  7. Return JWT.
//
// All rejection paths return AuthRejection with a stable Reason string
// so metrics can count by reason class without parsing free-form text.
func (uc *ProcessAuthCallout) Execute(
	ctx context.Context,
	in AuthCalloutInput,
) (AuthCalloutResult, error) {
	now := uc.now()

	enroll, err := uc.authCalloutEnrollment(ctx, in.EnrollmentToken, now)
	if err != nil {
		return AuthCalloutResult{}, err
	}

	// (3) worker name pattern match.
	if !enroll.MatchesWorkerName(in.WorkerName) {
		return AuthCalloutResult{}, &AuthRejection{
			Reason: "name_mismatch",
			Detail: fmt.Sprintf("worker name %q does not match pattern %q", in.WorkerName, enroll.WorkerNamePattern),
			Cause:  domain.ErrEnrollmentNameMismatch,
		}
	}

	// (4) user-nkey format. NATS will reject malformed keys later, but
	// we check first so we don't issue a JWT that won't bind.
	if !validUserNKey(in.UserNKeyPublic) {
		return AuthCalloutResult{}, &AuthRejection{
			Reason: "invalid_nkey",
			Detail: "user nkey malformed (expected 56-char 'U...' encoded Ed25519 public key)",
		}
	}

	// (5) mint the JWT. The issuer signs with the active account seed
	// from SigningKeyRepo (internal to the adapter).
	// Legacy pattern-only enrollments have no Name: the matched CONNECT name
	// is the pool name.
	name := enroll.Name
	if name == "" {
		name = in.WorkerName
	}
	perms := domain.StandardPermissionTemplate(enroll.Provider, name)
	credOut, err := uc.issuer.IssueWorkerCredential(ctx, port.IssueCredentialInput{
		UserNKeyPublic: in.UserNKeyPublic,
		Permissions:    perms,
		TTL:            uc.defaultJWTTTL,
		Bearer:         false,
	})
	if err != nil {
		return AuthCalloutResult{}, &AuthRejection{
			Reason: "internal",
			Detail: "JWT issuance failed",
			Cause:  err,
		}
	}

	grantID, err := uc.authCalloutPersistGrant(ctx, in, enroll.ID, now, credOut.ExpiresAt)
	if err != nil {
		return AuthCalloutResult{}, err
	}

	return AuthCalloutResult{
		UserJWT:          credOut.UserJWT,
		GrantID:          grantID,
		EnrollmentID:     enroll.ID,
		ExpiresAt:        credOut.ExpiresAt,
		AccountPublicKey: credOut.AccountPublicKey,
	}, nil
}

// authCalloutPersistGrant runs step 6 and returns the new grant id.
func (uc *ProcessAuthCallout) authCalloutPersistGrant(
	ctx context.Context,
	in AuthCalloutInput,
	enrollmentID domain.WorkerEnrollmentID,
	now, expiresAt time.Time,
) (domain.WorkerCredentialGrantID, error) {
	// (6) atomic: enrollment.uses++ and create grant row.
	grantID := domain.WorkerCredentialGrantID(uc.newID())
	instanceID := in.WorkerInstanceID
	if instanceID == "" {
		instanceID = uc.newID().String()
	}

	grant := domain.WorkerCredentialGrant{
		ID:               grantID,
		EnrollmentID:     enrollmentID,
		UserNKeyPublic:   in.UserNKeyPublic,
		WorkerName:       in.WorkerName,
		WorkerInstanceID: instanceID,
		WorkerVersion:    in.WorkerVersion,
		ClientHost:       in.ClientHost,
		IssuedAt:         now,
		ExpiresAt:        expiresAt,
	}

	err := uc.tx.InTx(ctx, func(ctx context.Context) error {
		if _, err := uc.enrollments.IncrementUses(ctx, enrollmentID); err != nil {
			return fmt.Errorf("increment uses: %w", err)
		}
		if _, err := uc.enrollments.CreateGrant(ctx, grant); err != nil {
			return fmt.Errorf("create grant: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.WorkerCredentialGrantID{}, &AuthRejection{
			Reason: "internal",
			Detail: "audit-trail persistence failed",
			Cause:  err,
		}
	}

	return grantID, nil
}

// authCalloutEnrollment runs steps 1–2: token lookup, lifecycle and provider validation.
func (uc *ProcessAuthCallout) authCalloutEnrollment(ctx context.Context, token string, now time.Time) (domain.WorkerEnrollment, error) {
	// (1) hash the token; the auth-callout receives plaintext.
	if token == "" {
		return domain.WorkerEnrollment{}, &AuthRejection{
			Reason: "no_token",
			Detail: "enrollment token missing from CONNECT options",
		}
	}
	hash := hashEnrollmentToken(token)

	enroll, err := uc.enrollments.FindEnrollmentByTokenHash(ctx, hash)
	if errors.Is(err, domain.ErrEnrollmentNotFound) || errors.Is(err, domain.ErrNotFound) {
		// Constant-time dummy compare to avoid a timing oracle that
		// distinguishes "token unknown" from "token known but invalid".
		_ = subtle.ConstantTimeCompare(hash, hash)
		return domain.WorkerEnrollment{}, &AuthRejection{
			Reason: "unknown_token",
			Detail: "token hash not found",
		}
	}
	if err != nil {
		return domain.WorkerEnrollment{}, &AuthRejection{
			Reason: "internal",
			Detail: "enrollment lookup failed",
			Cause:  err,
		}
	}

	// (2) validate enrollment lifecycle.
	if err := enroll.IsValidAt(now); err != nil {
		reason := "invalid"
		switch {
		case errors.Is(err, domain.ErrEnrollmentRevoked):
			reason = "revoked"
		case errors.Is(err, domain.ErrEnrollmentExpired):
			reason = "expired"
		case errors.Is(err, domain.ErrEnrollmentExhausted):
			reason = "exhausted"
		}
		return domain.WorkerEnrollment{}, &AuthRejection{
			Reason: reason,
			Detail: "enrollment failed lifecycle check",
			Cause:  err,
		}
	}

	// Pre-v0.3.1 rows may lack a provider; their permissions would be empty.
	if enroll.NeedsReenroll() {
		id := enroll.ID
		return domain.WorkerEnrollment{}, &AuthRejection{
			Reason:       "enrollment_missing_provider",
			Detail:       fmt.Sprintf("enrollment %s has no provider; re-enroll the worker", id),
			Cause:        domain.ErrEnrollmentMissingProvider,
			EnrollmentID: &id,
		}
	}

	return enroll, nil
}

// hashEnrollmentToken is the canonical token-to-hash function. Used both
// at creation time (CreateWorkerEnrollment) and at validation time
// (ProcessAuthCallout). Keep in sync; do NOT change to e.g. a salted
// hash without a migration.
func hashEnrollmentToken(plain string) []byte {
	sum := sha256.Sum256([]byte(plain))
	return sum[:]
}

// HashEnrollmentToken is the exported wrapper, useful for tests and CLI
// utilities that want to compute the hash without re-importing crypto.
func HashEnrollmentToken(plain string) []byte {
	return hashEnrollmentToken(plain)
}

// validUserNKey checks NATS' encoding: 56 chars, "U" prefix, base32
// alphabet. NATS will recheck on its side with the crc16 trailer; we
// don't bother since the issuer will fail fast on bad keys.
func validUserNKey(s string) bool {
	if len(s) != 56 {
		return false
	}
	if s[0] != 'U' {
		return false
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	for _, c := range s[1:] {
		ok := false
		for _, a := range alphabet {
			if c == a {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// RevokeWorkerEnrollment
//
// Admin-side. Marks the enrollment as revoked. Existing connections are
// not killed by this call (they continue using their already-minted
// JWT until expiry). To kick a worker immediately, the admin also calls
// RevokeWorkerCredentialGrant for the active grant.
// ---------------------------------------------------------------------------

type RevokeWorkerEnrollment struct {
	enrollments port.WorkerEnrollmentRepo
	now         func() time.Time
}

func NewRevokeWorkerEnrollment(
	enrollments port.WorkerEnrollmentRepo,
	now func() time.Time,
) *RevokeWorkerEnrollment {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &RevokeWorkerEnrollment{enrollments: enrollments, now: now}
}

type RevokeInput struct {
	EnrollmentID domain.WorkerEnrollmentID
	By           *domain.UserID
	Reason       string
}

func (uc *RevokeWorkerEnrollment) Execute(ctx context.Context, in RevokeInput) error {
	if in.Reason == "" {
		in.Reason = "admin_revoke"
	}
	return uc.enrollments.RevokeEnrollment(ctx, in.EnrollmentID, in.By, in.Reason, uc.now())
}
