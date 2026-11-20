//go:build integration

package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	cairnv1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/gen/proto/cairn/v1/cairnv1connect"
	"github.com/opencairn-org/cairn-core/internal/domain"
)

// TestAPI_ActivityEventStream holds StreamActivityEvents open over HTTP: the
// stream is established before any event, and a manual entry made afterwards
// arrives on it as IMPORT_COMPLETED.
func TestAPI_ActivityEventStream(t *testing.T) {
	env := newITestEnv(t)
	alice := env.user("alice", domain.UserRoleUser)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client := cairnv1connect.NewActivityServiceClient(connectrpc.NewClient(connecthttp.NewTransport(alice.http, env.srv.URL)))
	// Returns once the response headers arrive, which the server sends after subscribing.
	stream, err := client.StreamActivityEvents(ctx, &cairnv1.StreamActivityEventsRequest{
		Types: []cairnv1.ActivityEventType{cairnv1.ActivityEventType_ACTIVITY_EVENT_TYPE_IMPORT_COMPLETED},
	})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = stream.Close() }()
	if n := env.app.ActivityEvents.Subscribers(alice.userID); n != 1 {
		t.Fatalf("subscribers after open = %d, want 1", n)
	}

	manual := alice.post("/api/activities/manual", map[string]any{
		"type": "run", "title": "Track session", "start_time": time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC).Format(time.RFC3339),
		"elapsed_duration_s": 3600, "moving_duration_s": 3500, "distance_m": 10000,
	}, http.StatusOK, http.StatusCreated)
	ev, err := stream.Receive()
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if ev.GetType() != cairnv1.ActivityEventType_ACTIVITY_EVENT_TYPE_IMPORT_COMPLETED || ev.GetActivityId() != str(manual, "activity_id") {
		t.Fatalf("event %v, want import completed for %s", ev, str(manual, "activity_id"))
	}
}
