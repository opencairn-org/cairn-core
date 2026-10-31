// Package imaging derives the display variants of an uploaded photo: a thumb
// and a medium JPEG plus a blurhash placeholder. Pure Go (standard library
// decoders, x/image for WebP decode and scaling, go-blurhash), so the static
// distroless build stays cgo-free.
package imaging

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // registered decoders: providers and phones upload any of these
	"image/jpeg"
	_ "image/png"

	"github.com/bbrks/go-blurhash"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// Variant edge sizes (longest side, px) and the JPEG quality of the variants.
const (
	ThumbMaxEdge  = 320
	MediumMaxEdge = 1280
	JPEGQuality   = 82
	// MaxPixels refuses decompression bombs: a 50 MP photo is the upper end
	// of what phones produce.
	MaxPixels = 50_000_000
)

// ErrTooLarge is returned for an image above MaxPixels.
var ErrTooLarge = errors.New("imaging: image exceeds the pixel limit")

// Variants are the derived representations of one photo.
type Variants struct {
	Width, Height int    // of the original
	Thumb         []byte // JPEG, longest side ≤ ThumbMaxEdge
	Medium        []byte // JPEG, longest side ≤ MediumMaxEdge; always re-encoded so it carries no metadata
	Blurhash      string // 4x3 component blurhash of the thumb
}

// Derive decodes raw and produces the variants. Orientation metadata is not
// applied (the variants keep the stored pixel orientation).
func Derive(raw []byte) (Variants, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return Variants{}, fmt.Errorf("imaging: decode config: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > MaxPixels {
		return Variants{}, ErrTooLarge
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return Variants{}, fmt.Errorf("imaging: decode: %w", err)
	}
	v := Variants{Width: cfg.Width, Height: cfg.Height}
	thumb := Scale(img, ThumbMaxEdge)
	if v.Thumb, err = EncodeJPEG(thumb); err != nil {
		return Variants{}, err
	}
	if v.Medium, err = EncodeJPEG(Scale(img, MediumMaxEdge)); err != nil {
		return Variants{}, err
	}
	// Blurhash from the thumb: the hash only encodes a few DCT components,
	// a 320 px source is plenty and far cheaper than the original.
	if v.Blurhash, err = blurhash.Encode(4, 3, thumb); err != nil {
		return Variants{}, fmt.Errorf("imaging: blurhash: %w", err)
	}
	return v, nil
}

// Scale fits img into a maxEdge square, keeping the aspect ratio. An image
// already within the bound is returned as is.
func Scale(img image.Image, maxEdge int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxEdge && h <= maxEdge {
		return img
	}
	if w >= h {
		h = max(1, h*maxEdge/w)
		w = maxEdge
	} else {
		w = max(1, w*maxEdge/h)
		h = maxEdge
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	return dst
}

// EncodeJPEG encodes img at the variant quality.
func EncodeJPEG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: JPEGQuality}); err != nil {
		return nil, fmt.Errorf("imaging: encode jpeg: %w", err)
	}
	return buf.Bytes(), nil
}
