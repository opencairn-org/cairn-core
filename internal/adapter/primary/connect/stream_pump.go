package connect

import (
	"context"
	"time"
)

// pumpStream runs a server stream until the client goes away or the feed
// closes: every heartbeat interval it sends a keepalive, and forwards each
// item of events through forward. A send error ends the stream.
func pumpStream[T any](ctx context.Context, events <-chan T, heartbeat func() error, forward func(T) error) error {
	ticker := time.NewTicker(streamHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := heartbeat(); err != nil {
				return err
			}
		case item, ok := <-events:
			if !ok {
				return nil
			}
			if err := forward(item); err != nil {
				return err
			}
		}
	}
}
