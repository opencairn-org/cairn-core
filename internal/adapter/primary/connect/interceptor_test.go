package connect

import (
	"context"
	"errors"
	"testing"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	authuc "github.com/opencairn-org/cairn-core/internal/usecase/auth"
)

func callInterceptor(t *testing.T, ic connectrpc.ServerInterceptor, ctx context.Context, spec connectrpc.Spec) (bool, error) {
	t.Helper()
	called := false
	err := ic(func(context.Context, connectrpc.Spec, connectrpc.ServerStream) error {
		called = true
		return nil
	})(ctx, spec, nil)
	return called, err
}

// Streams go through the same auth chain as unary calls: an anonymous stream
// stays anonymous (handlers refuse), a read-verb stream passes the scope check
// and the admin gate refuses it outright.
func TestInterceptorsCoverStreams(t *testing.T) {
	spec := connectrpc.Spec{StreamType: connectrpc.StreamTypeServer, Procedure: "/cairn.v1.AdminService/StreamAdminEvents"}
	for name, ic := range map[string]connectrpc.ServerInterceptor{
		"session": SessionInterceptor(nil, "cairn_session"),
		"scope":   ScopeInterceptor(),
	} {
		if called, err := callInterceptor(t, ic, t.Context(), spec); !called || err != nil {
			t.Errorf("%s: anonymous stream must reach the handler (called=%v err=%v)", name, called, err)
		}
	}
	called, err := callInterceptor(t, RequireAdminInterceptor(nil), t.Context(), spec)
	if called || connectrpc.CodeOf(err) != connectrpc.CodeUnauthenticated {
		t.Errorf("admin: anonymous stream called=%v err=%v, want CodeUnauthenticated", called, err)
	}
	// A read-only OAuth token may not open a mutating stream either.
	ctx := context.WithValue(t.Context(), ctxKeyScopes, []string{"activities:read"})
	mut := connectrpc.Spec{StreamType: connectrpc.StreamTypeClient, Procedure: "/cairn.v1.ActivityService/UploadActivityChunks"}
	if called, err := callInterceptor(t, ScopeInterceptor(), ctx, mut); called || connectrpc.CodeOf(err) != connectrpc.CodePermissionDenied {
		t.Errorf("scope: mutating stream called=%v err=%v, want CodePermissionDenied", called, err)
	}
}

func TestRequireAdminInterceptorUnauthenticated(t *testing.T) {
	spec := connectrpc.Spec{StreamType: connectrpc.StreamTypeUnary, Procedure: "/cairn.v1.AdminService/ListUsers"}
	called, err := callInterceptor(t, RequireAdminInterceptor(nil), t.Context(), spec)
	if called || connectrpc.CodeOf(err) != connectrpc.CodeUnauthenticated {
		t.Fatalf("called=%v err=%v, want CodeUnauthenticated", called, err)
	}
}

func TestScopeInterceptor(t *testing.T) {
	ctx := context.WithValue(t.Context(), ctxKeyScopes, []string{"activities:read"})
	read := connectrpc.Spec{StreamType: connectrpc.StreamTypeUnary, Procedure: "/cairn.v1.ActivityService/GetActivity"}
	if called, err := callInterceptor(t, ScopeInterceptor(), ctx, read); !called || err != nil {
		t.Fatalf("read: called=%v err=%v", called, err)
	}
	write := connectrpc.Spec{StreamType: connectrpc.StreamTypeUnary, Procedure: "/cairn.v1.ActivityService/UpdateActivity"}
	if called, err := callInterceptor(t, ScopeInterceptor(), ctx, write); called || connectrpc.CodeOf(err) != connectrpc.CodePermissionDenied {
		t.Fatalf("write: called=%v err=%v, want CodePermissionDenied", called, err)
	}
}

func TestConnectErrorKeepsMessageAndCause(t *testing.T) {
	err := notFoundErr(domain.ErrNotFound)
	var cerr *connectrpc.Error
	if !errors.As(err, &cerr) || cerr.Code() != connectrpc.CodeNotFound || cerr.Message() != domain.ErrNotFound.Error() {
		t.Fatalf("got %v", err)
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("cause lost")
	}
}

func TestRequestHeaderWithoutCallInfo(t *testing.T) {
	if got := requestHeader(t.Context()).Get("Cookie"); got != "" {
		t.Fatalf("got %q", got)
	}
}

type icSessions struct {
	port.SessionRepo
	sess    domain.Session
	touched int
}

func (f *icSessions) GetByTokenHash(context.Context, []byte) (domain.Session, error) {
	return f.sess, nil
}

func (f *icSessions) TouchLastSeen(context.Context, domain.SessionID) error {
	f.touched++
	return nil
}

type icOAuth struct{ tok domain.OAuthAccessToken }

func (f icOAuth) FindAccessToken(context.Context, []byte) (domain.OAuthAccessToken, error) {
	return f.tok, nil
}

// runSessionInterceptor dispatches one call with headers and returns the handler's context.
func runSessionInterceptor(t *testing.T, callers *authuc.Callers, headers map[string]string) context.Context {
	t.Helper()
	var seen context.Context
	srv := connectrpc.NewServer(SessionInterceptor(callers, "cairn_session"))
	srv.SetUnknownHandler(func(ctx context.Context, _ connectrpc.Spec, _ connectrpc.ServerStream) error {
		seen = ctx
		return nil
	})
	info := &connectrpc.CallInfo{}
	for k, v := range headers {
		info.RequestHeader().Set(k, v)
	}
	if err := srv.Call(t.Context(), "/cairn.v1.AuthService/GetCurrentUser", info, nil); err != nil {
		t.Fatal(err)
	}
	return seen
}

// Cookie and cairn_ses_ bearer sessions stash a session id and touch last-seen; OAuth stashes scopes.
func TestSessionInterceptorStashesCaller(t *testing.T) {
	uid := domain.UserID(uuid.New())
	sid := domain.SessionID(uuid.New())
	sessions := &icSessions{sess: domain.Session{ID: sid, UserID: uid, ExpiresAt: time.Now().Add(time.Hour)}}
	oauth := icOAuth{tok: domain.OAuthAccessToken{UserID: uid, Scope: "activities:read", ExpiresAt: time.Now().Add(time.Hour)}}
	callers := &authuc.Callers{Sessions: sessions, OAuth: oauth}

	for name, h := range map[string]map[string]string{
		"cookie": {"Cookie": "other=1; cairn_session=abc"},
		"bearer": {"Authorization": "Bearer " + domain.SessionTokenPrefix + "abc"},
	} {
		ctx := runSessionInterceptor(t, callers, h)
		got, err := userIDFromHandlerCtx(ctx)
		s, ok := sessionIDFromHandlerCtx(ctx)
		if err != nil || got != uid || !ok || s != sid || scopesFromHandlerCtx(ctx) != nil {
			t.Errorf("%s: uid=%v err=%v sid=%v ok=%v", name, got, err, s, ok)
		}
	}
	if sessions.touched != 2 {
		t.Errorf("touched %d sessions, want 2", sessions.touched)
	}

	ctx := runSessionInterceptor(t, callers, map[string]string{"Authorization": "Bearer " + domain.OAuthAccessTokenPrefix + "abc"})
	if scopes := scopesFromHandlerCtx(ctx); len(scopes) != 1 || scopes[0] != "activities:read" {
		t.Fatalf("scopes = %v", scopes)
	}
	if _, ok := sessionIDFromHandlerCtx(ctx); ok {
		t.Fatal("oauth caller carries a session id")
	}

	ctx = runSessionInterceptor(t, callers, nil)
	if _, err := userIDFromHandlerCtx(ctx); connectrpc.CodeOf(err) != connectrpc.CodeUnauthenticated {
		t.Fatalf("anonymous err = %v", err)
	}
}
