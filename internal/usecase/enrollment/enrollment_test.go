package enrollment

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

type memRepo struct {
	port.WorkerEnrollmentRepo
	rows   []domain.WorkerEnrollment
	grants int
}

func (m *memRepo) CreateEnrollment(_ context.Context, e domain.WorkerEnrollment) (domain.WorkerEnrollment, error) {
	m.rows = append(m.rows, e)
	return e, nil
}

func (m *memRepo) ListEnrollments(context.Context, port.ListEnrollmentsFilter) ([]domain.WorkerEnrollment, error) {
	return m.rows, nil
}

func (m *memRepo) FindEnrollmentByTokenHash(_ context.Context, hash []byte) (domain.WorkerEnrollment, error) {
	for _, e := range m.rows {
		if string(e.TokenHash) == string(hash) {
			return e, nil
		}
	}
	return domain.WorkerEnrollment{}, domain.ErrEnrollmentNotFound
}

func (m *memRepo) IncrementUses(context.Context, domain.WorkerEnrollmentID) (int, error) {
	return 1, nil
}

func (m *memRepo) CreateGrant(_ context.Context, g domain.WorkerCredentialGrant) (domain.WorkerCredentialGrant, error) {
	m.grants++
	return g, nil
}

type recIssuer struct {
	port.NATSCredentialIssuer
	calls []port.IssueCredentialInput
}

func (r *recIssuer) IssueWorkerCredential(_ context.Context, in port.IssueCredentialInput) (port.IssueCredentialOutput, error) {
	r.calls = append(r.calls, in)
	return port.IssueCredentialOutput{UserJWT: "jwt", ExpiresAt: time.Now().Add(in.TTL)}, nil
}

type inline struct{}

func (inline) InTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }

const testNKey = "UAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAB"

func TestCreateEnrollmentRequiresProvider(t *testing.T) {
	uc := NewCreateWorkerEnrollment(&memRepo{}, nil, nil, nil)
	for _, p := range []string{"", "Strava", "a.b", "a>"} {
		_, err := uc.Execute(context.Background(), CreateEnrollmentInput{Name: "w1", Provider: p, ExpiresIn: time.Hour})
		if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "provider") {
			t.Errorf("provider %q: want provider validation error, got %v", p, err)
		}
	}
	res, err := uc.Execute(context.Background(), CreateEnrollmentInput{Name: "w1", Provider: "strava", ExpiresIn: time.Hour})
	if err != nil || res.Enrollment.Provider != "strava" || res.Token == "" {
		t.Fatalf("create: %+v %v", res, err)
	}
}

func TestAuthCalloutRejectsMissingProvider(t *testing.T) {
	repo := &memRepo{}
	issuer := &recIssuer{}
	id := domain.WorkerEnrollmentID(uuid.New())
	repo.rows = append(repo.rows, domain.WorkerEnrollment{
		ID: id, TokenHash: HashEnrollmentToken("tok"), Name: "w1", ExpiresAt: time.Now().Add(time.Hour),
	})
	_, err := NewProcessAuthCallout(repo, issuer, inline{}, nil, nil, 0).Execute(context.Background(), AuthCalloutInput{
		EnrollmentToken: "tok", UserNKeyPublic: testNKey, WorkerName: "w1",
	})
	var rej *AuthRejection
	if !errors.As(err, &rej) || rej.Reason != "enrollment_missing_provider" {
		t.Fatalf("want enrollment_missing_provider rejection, got %v", err)
	}
	if !errors.Is(err, domain.ErrEnrollmentMissingProvider) || rej.EnrollmentID == nil || *rej.EnrollmentID != id {
		t.Errorf("rejection must carry cause and enrollment id: %+v", rej)
	}
	if want := "enrollment " + id.String() + " has no provider; re-enroll the worker"; rej.Detail != want {
		t.Errorf("detail = %q, want %q", rej.Detail, want)
	}
	if len(issuer.calls) != 0 || repo.grants != 0 {
		t.Errorf("no credential may be issued or recorded: %d issued, %d grants", len(issuer.calls), repo.grants)
	}
}

func TestAuthCalloutIssuesProviderTemplate(t *testing.T) {
	repo := &memRepo{}
	issuer := &recIssuer{}
	repo.rows = append(repo.rows, domain.WorkerEnrollment{
		ID: domain.WorkerEnrollmentID(uuid.New()), TokenHash: HashEnrollmentToken("tok"),
		Name: "w1", Provider: "strava", ExpiresAt: time.Now().Add(time.Hour),
	})
	res, err := NewProcessAuthCallout(repo, issuer, inline{}, nil, nil, 0).Execute(context.Background(), AuthCalloutInput{
		EnrollmentToken: "tok", UserNKeyPublic: testNKey, WorkerName: "w1",
	})
	if err != nil || res.UserJWT != "jwt" {
		t.Fatalf("admit: %+v %v", res, err)
	}
	if len(issuer.calls) != 1 || len(issuer.calls[0].Permissions.AllowSubscribe) == 0 || repo.grants != 1 {
		t.Fatalf("want one scoped issuance and grant: %+v, %d grants", issuer.calls, repo.grants)
	}
}

func TestCreateEnrollment_RefusesCollidingWorkerKey(t *testing.T) {
	repo := &memRepo{}
	uc := NewCreateWorkerEnrollment(repo, nil, nil, nil)
	ctx := context.Background()
	if _, err := uc.Execute(ctx, CreateEnrollmentInput{Provider: "c", Name: "a-b", ExpiresIn: time.Hour}); err != nil {
		t.Fatal(err)
	}
	// "a" + "b-c" also yields the key "a-b-c".
	if _, err := uc.Execute(ctx, CreateEnrollmentInput{Provider: "b-c", Name: "a", ExpiresIn: time.Hour}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
	// The same pool may be enrolled again (another instance's token).
	if _, err := uc.Execute(ctx, CreateEnrollmentInput{Provider: "c", Name: "a-b", ExpiresIn: time.Hour}); err != nil {
		t.Fatal(err)
	}
}
