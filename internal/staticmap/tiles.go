package staticmap

import (
	"context"
	"fmt"
	"image"
	_ "image/jpeg" // tile providers may serve JPEG
	_ "image/png"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HTTPTileFetcher fetches raster tiles from an XYZ URL template
// ({z}/{x}/{y}). Render-time only: rendered snapshots are cached by the caller.
func HTTPTileFetcher(urlTemplate, userAgent string, timeout time.Duration) TileFetcher {
	client := &http.Client{Timeout: timeout}
	return func(ctx context.Context, z, x, y int) (image.Image, error) {
		r := strings.NewReplacer("{z}", strconv.Itoa(z), "{x}", strconv.Itoa(x), "{y}", strconv.Itoa(y))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.Replace(urlTemplate), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("tile %d/%d/%d: status %d", z, x, y, resp.StatusCode)
		}
		// image.Decode rather than png.Decode: providers serve PNG or JPEG.
		img, _, err := image.Decode(resp.Body)
		return img, err
	}
}
