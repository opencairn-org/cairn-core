package imaging

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func pngOf(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func TestDeriveLargePhoto(t *testing.T) {
	v, err := Derive(pngOf(2000, 1000))
	if err != nil {
		t.Fatal(err)
	}
	if v.Width != 2000 || v.Height != 1000 || v.Blurhash == "" {
		t.Fatalf("variants %+v", v)
	}
	for name, data := range map[string][]byte{"thumb": v.Thumb, "medium": v.Medium} {
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if cfg.Width != map[string]int{"thumb": ThumbMaxEdge, "medium": MediumMaxEdge}[name] || cfg.Height != cfg.Width/2 {
			t.Errorf("%s: %dx%d", name, cfg.Width, cfg.Height)
		}
	}
}

func TestDeriveSmallPhotoKeepsNoMedium(t *testing.T) {
	v, err := Derive(pngOf(300, 200))
	if err != nil {
		t.Fatal(err)
	}
	if v.Medium != nil {
		t.Fatal("an original within the medium bound needs no medium variant")
	}
	cfg, _, _ := image.DecodeConfig(bytes.NewReader(v.Thumb))
	if cfg.Width != 300 || cfg.Height != 200 {
		t.Fatalf("thumb must keep a small original's size, got %dx%d", cfg.Width, cfg.Height)
	}
}

func TestDeriveRejectsGarbageAndBombs(t *testing.T) {
	if _, err := Derive([]byte("not an image")); err == nil {
		t.Fatal("garbage must fail")
	}
	// A PNG header claiming 20000x20000 trips the pixel limit before decoding.
	hdr := pngOf(1, 1)
	binary.BigEndian.PutUint32(hdr[16:], 20000)                          // width
	binary.BigEndian.PutUint32(hdr[20:], 20000)                          // height
	binary.BigEndian.PutUint32(hdr[29:], crc32.ChecksumIEEE(hdr[12:29])) // IHDR chunk crc
	if _, err := Derive(hdr); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}
