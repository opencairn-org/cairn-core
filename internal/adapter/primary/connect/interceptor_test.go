package connect

import (
	"context"
	"errors"
	"testing"

	connectrpc "connectrpc.com/connect/v2"

	"github.com/opencairn-org/cairn-core/internal/domain"
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
		"session": SessionInterceptor(nil, nil, nil, "cairn_session"),
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
