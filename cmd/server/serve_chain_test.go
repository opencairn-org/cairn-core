package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	connectrpc "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	cairnv1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/gen/proto/cairn/v1/cairnv1connect"
)

// heldStream sends one event, holds the stream open until release, then sends a second.
type heldStream struct {
	cairnv1connect.UnimplementedActivityServiceHandler
	release <-chan struct{}
}

func (h heldStream) StreamActivityEvents(ctx context.Context, _ *cairnv1.StreamActivityEventsRequest, stream cairnv1connect.ActivityServiceStreamActivityEventsServerStream) error {
	if err := stream.Send(&cairnv1.ActivityEvent{I18NKey: "first"}); err != nil {
		return err
	}
	select {
	case <-h.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return stream.Send(&cairnv1.ActivityEvent{I18NKey: "second"})
}

// A server stream through the production middleware chain delivers each
// message as it is sent and outlives the server's WriteTimeout.
func TestServerStreamThroughHandlerChain(t *testing.T) {
	const writeTimeout = 200 * time.Millisecond
	release := make(chan struct{})
	server := connectrpc.NewServer()
	cairnv1connect.RegisterActivityServiceHandler(server, heldStream{release: release})
	mux := http.NewServeMux()
	connecthttp.Mount(streamSafeMux{mux}, server)
	srv := httptest.NewUnstartedServer(serveHandlerChain(newIPRateLimiter(600, nil), mux))
	srv.Config.WriteTimeout = writeTimeout
	srv.Start()
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client := cairnv1connect.NewActivityServiceClient(connectrpc.NewClient(connecthttp.NewTransport(srv.Client(), srv.URL)))
	stream, err := client.StreamActivityEvents(ctx, &cairnv1.StreamActivityEventsRequest{})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	// The handler is still holding the stream: this arrives only if it was flushed.
	ev, err := stream.Receive()
	if err != nil || ev.GetI18NKey() != "first" {
		t.Fatalf("first event: %v, %v", ev, err)
	}
	time.Sleep(2 * writeTimeout)
	close(release)
	ev, err = stream.Receive()
	if err != nil || ev.GetI18NKey() != "second" {
		t.Fatalf("event after the write timeout: %v, %v", ev, err)
	}
	if _, err := stream.Receive(); !errors.Is(err, io.EOF) {
		t.Fatalf("stream end: %v", err)
	}
}
