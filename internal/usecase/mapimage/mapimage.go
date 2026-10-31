// Package mapimage renders and caches the static PNG snapshot of an
// activity's course (the GPS polyline over a basemap) for feeds, thumbnails
// and federation. Non-owner variants drop the track inside the owner's
// privacy zones.
package mapimage

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/staticmap"
)

// StyleVersion namespaces the blob cache. Bump it whenever the rendered style
// changes (colours, markers, tile density) so stale snapshots are bypassed.
// v2: orange casing + start/finish markers + retina tiles.
// v3: 2:1 landscape framing to fit the feed thumbnail boxes without cropping.
// v4: 3:1 short banner so the snapshot takes less vertical space in feeds.
// v5: basemap moved off CARTO (its free tiles became "API KEY REQUIRED"
// watermarks) to the configurable CAIRN_MAP_TILE_URL source.
const StyleVersion = "v5"

// maxPoints caps the rendered polyline.
const maxPoints = 600

// ZoneSource yields the owner's privacy zones for a non-owner render
// (view.Visibility.OwnerZones).
type ZoneSource interface {
	OwnerZones(ctx context.Context, ownerID domain.UserID, isOwner bool) []domain.PrivacyZone
}

// Images renders snapshots and caches them in the blob store.
type Images struct {
	streams   port.StreamRepo
	blobs     port.BlobStore // nil: render every time, cache nothing
	zones     ZoneSource     // nil: no trimming possible
	fetchTile staticmap.TileFetcher
	logger    *slog.Logger
}

// New wires the renderer; blobs and zones may be nil.
func New(streams port.StreamRepo, blobs port.BlobStore, zones ZoneSource, fetchTile staticmap.TileFetcher, logger *slog.Logger) *Images {
	if logger == nil {
		logger = slog.Default()
	}
	return &Images{streams: streams, blobs: blobs, zones: zones, fetchTile: fetchTile, logger: logger}
}

// CacheKey is the blob key of one variant's snapshot.
func CacheKey(id domain.ActivityID, trimmed bool) string {
	variant := "full"
	if trimmed {
		variant = "trimmed"
	}
	return "mapimg/" + StyleVersion + "/" + id.String() + "/" + variant + ".png"
}

// ETag changes when the course image would render differently; empty when
// the activity has no track.
func ETag(act domain.Activity) string {
	if act.PrimaryStreamSourceID == nil {
		return ""
	}
	return StyleVersion + "-" + act.PrimaryStreamSourceID.String()[:8] + "-" + strconv.FormatInt(act.MergedAt.Unix(), 36)
}

// Load serves the cached snapshot of the variant or renders and caches it.
// ok is false when there is no renderable track.
func (m *Images) Load(ctx context.Context, act domain.Activity, trimmed bool) ([]byte, bool) {
	key := CacheKey(act.ID, trimmed)
	if m.blobs != nil {
		if data, _, err := m.blobs.Get(ctx, key); err == nil && len(data) > 0 {
			return data, true
		}
	}
	data, ok := m.render(ctx, act, trimmed)
	if !ok {
		return nil, false
	}
	m.store(ctx, act.ID, key, data)
	return data, true
}

// Regenerate drops both cached variants and renders the owner's afresh (the
// trimmed one re-renders lazily). For when the track changed under the cache
// or a render failed and cached nothing. false: no renderable track.
func (m *Images) Regenerate(ctx context.Context, act domain.Activity) bool {
	if m.blobs != nil {
		for _, trimmed := range []bool{false, true} {
			if err := m.blobs.Delete(ctx, CacheKey(act.ID, trimmed)); err != nil {
				m.logger.Warn("map image: cache delete failed", "activity", act.ID, "trimmed", trimmed, "error", err)
			}
		}
	}
	data, ok := m.render(ctx, act, false)
	if !ok {
		return false
	}
	m.store(ctx, act.ID, CacheKey(act.ID, false), data)
	return true
}

func (m *Images) store(ctx context.Context, id domain.ActivityID, key string, data []byte) {
	if m.blobs == nil {
		return
	}
	if err := m.blobs.Put(ctx, key, data, "image/png"); err != nil {
		m.logger.Warn("map image: cache store failed", "activity", id, "error", err)
	}
}

// render reads the GPS track, optionally trims it against the owner's privacy
// zones and draws the PNG. ok=false when no renderable track is left.
func (m *Images) render(ctx context.Context, act domain.Activity, trim bool) ([]byte, bool) {
	if act.PrimaryStreamSourceID == nil || m.streams == nil {
		return nil, false
	}
	stream, err := m.streams.QueryStream(ctx, domain.StreamQuery{
		ActivitySourceID: *act.PrimaryStreamSourceID,
		Channels:         []domain.StreamChannel{domain.StreamChannelLatitude, domain.StreamChannelLongitude},
		Resolution:       domain.StreamResolutionRaw,
	})
	if err != nil {
		m.logger.Warn("map image: stream query failed", "activity", act.ID, "error", err)
		return nil, false
	}
	var zones []domain.PrivacyZone
	if m.zones != nil {
		zones = m.zones.OwnerZones(ctx, act.UserID, !trim)
	}
	pts := make([]staticmap.LatLng, 0, len(stream.Samples))
	for _, s := range stream.Samples {
		if s.Latitude == nil || s.Longitude == nil {
			continue
		}
		if len(zones) > 0 && domain.PointInAnyZone(zones, *s.Latitude, *s.Longitude) {
			continue // drop track segments inside a privacy zone
		}
		pts = append(pts, staticmap.LatLng{Lat: *s.Latitude, Lng: *s.Longitude})
	}
	pts = downsample(pts, maxPoints)
	if len(pts) < 2 {
		return nil, false
	}
	var buf bytes.Buffer
	if err := staticmap.RenderPNG(ctx, &buf, pts, staticmap.DefaultOptions(), m.fetchTile); err != nil {
		m.logger.Warn("map image: render failed", "activity", act.ID, "error", err)
		return nil, false
	}
	return buf.Bytes(), true
}

// downsample reduces a dense track to at most limit points by even stride,
// always keeping the first and last.
func downsample(pts []staticmap.LatLng, limit int) []staticmap.LatLng {
	if len(pts) <= limit || limit < 2 {
		return pts
	}
	stride := float64(len(pts)-1) / float64(limit-1)
	out := make([]staticmap.LatLng, 0, limit)
	for i := range limit {
		out = append(out, pts[int(float64(i)*stride)])
	}
	return out
}
