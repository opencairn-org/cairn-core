package connect

import (
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/domain/streamalign"
)

// streamalignFixture builds a two-point merged stream with one speed channel.
func streamalignFixture(sid domain.SourceID, speed []*float64) streamalign.MergedStream {
	t0 := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	return streamalign.MergedStream{
		Grid:       []time.Time{t0, t0.Add(5 * time.Second)},
		Channels:   map[domain.StreamChannel][]*float64{domain.StreamChannelSpeed: speed},
		Provenance: map[domain.StreamChannel]domain.SourceID{domain.StreamChannelSpeed: sid},
	}
}
