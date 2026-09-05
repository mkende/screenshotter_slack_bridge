// Package imageproc optionally downscales PNG screenshots before they are
// uploaded to Slack. It mirrors the server's high-quality CatmullRom scaling.
package imageproc

import (
	"bytes"
	"fmt"
	"image"
	"image/png"

	"golang.org/x/image/draw"
)

const (
	// maxImageDim and maxImagePixels guard the PNG decoder against
	// decompression bombs, matching the limits enforced by the server.
	maxImageDim    = 20000
	maxImagePixels = 40_000_000
)

// MaybeResize returns pngData unchanged when maxDim <= 0 or the image already
// fits within maxDim on its longest side. Otherwise it decodes the PNG,
// downscales it preserving aspect ratio, and re-encodes it as PNG.
func MaybeResize(pngData []byte, maxDim int) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(pngData))
	if err != nil {
		return nil, fmt.Errorf("decode image header: %w", err)
	}
	if cfg.Width > maxImageDim || cfg.Height > maxImageDim || cfg.Width*cfg.Height > maxImagePixels {
		return nil, fmt.Errorf("image dimensions exceed the allowed maximum (%dx%d px or %d Mpx total)", maxImageDim, maxImageDim, maxImagePixels/1_000_000)
	}

	if maxDim <= 0 || (cfg.Width <= maxDim && cfg.Height <= maxDim) {
		return pngData, nil
	}

	src, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	dst := scaleDown(src, maxDim)

	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, fmt.Errorf("encode image: %w", err)
	}
	return buf.Bytes(), nil
}

// scaleDown returns a copy of src scaled so its longest side is at most maxDim,
// preserving aspect ratio. src is assumed to already exceed maxDim.
func scaleDown(src image.Image, maxDim int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()

	tw, th := w, h
	if w >= h {
		tw = maxDim
		th = h * maxDim / w
	} else {
		th = maxDim
		tw = w * maxDim / h
	}
	if tw < 1 {
		tw = 1
	}
	if th < 1 {
		th = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}
