package connect

import (
	"context"
	"time"
)

// pumpStream runs a server stream until the client goes away or the feed
// closes: every heartbeat interval it sends a keepalive, and forwards each
// item of events through forward. A send error ends the stream.
//
// open sends the response headers before the first event, so a client waiting
// on them sees the stream established at once rather than at the first event
// or heartbeat. Call pumpStream after subscribing: headers then mean "live".
func pumpStream[T any](ctx context.Context, events <-chan T, open, heartbeat func() error, forward func(T) error) error {
	if err := open(); err != nil {
		return err
	}
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
