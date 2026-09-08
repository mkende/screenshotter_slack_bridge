// Package imageproc turns a PNG screenshot into an image Slack accepts as the
// preview_image of a remote file. It mirrors the server's high-quality
// CatmullRom scaling (see server/internal/storage/storage.go); the code is
// duplicated rather than shared because the bridge is a separate Go module and
// the server is published on its own as a self-contained repository.
package imageproc

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/png"

	xdraw "golang.org/x/image/draw"
)

const (
	// maxImageDim and maxImagePixels guard the PNG decoder against
	// decompression bombs, matching the limits enforced by the server.
	maxImageDim    = 20000
	maxImagePixels = 40_000_000

	// MinPreviewDim is the smallest side files.remote.add accepts on a preview
	// image, exported so the configuration can reject a max_dimension below it.
	// Slack documents a "minimum of 800w x 400h" but actually enforces 300 px on
	// *each* side: 300x300 is accepted, while 299x299, 280x1000 and 800x100 are
	// all rejected with invalid_preview_dimensions.
	MinPreviewDim = 300

	// maxPreviewUpscale bounds how far a small screenshot is enlarged to reach
	// MinPreviewDim. Beyond it the result is more blur than detail, so whatever
	// the bounded upscale does not cover is padded instead. This matters for
	// thin crops, where scaling alone would need 7x or more.
	maxPreviewUpscale = 3
)

// padColor fills the area around a screenshot too small to reach MinPreviewDim
// by scaling alone. Opaque white keeps the card readable in both Slack themes
// and matches the background of most screenshots.
var padColor = image.NewUniform(image.White.C)

// PreparePreview returns pngData as a preview image valid for
// files.remote.add: at most maxDim on its longest side when maxDim is positive,
// and at least 300 px on each side. The screenshot is scaled with its aspect
// ratio preserved — never distorted — and is enlarged by at most 3x, so a crop
// too thin to reach the minimum that way is centered on a white canvas instead.
// pngData is returned unchanged when it already satisfies both bounds.
func PreparePreview(pngData []byte, maxDim int) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(pngData))
	if err != nil {
		return nil, fmt.Errorf("decode image header: %w", err)
	}
	if cfg.Width > maxImageDim || cfg.Height > maxImageDim || cfg.Width*cfg.Height > maxImagePixels {
		return nil, fmt.Errorf("image dimensions exceed the allowed maximum (%dx%d px or %d Mpx total)", maxImageDim, maxImageDim, maxImagePixels/1_000_000)
	}

	w, h := previewSize(cfg.Width, cfg.Height, maxDim)
	canvasW, canvasH := max(w, MinPreviewDim), max(h, MinPreviewDim)
	if w == cfg.Width && h == cfg.Height && canvasW == w && canvasH == h {
		return pngData, nil
	}

	src, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}

	dst := image.NewRGBA(image.Rect(0, 0, canvasW, canvasH))
	if canvasW != w || canvasH != h {
		draw.Draw(dst, dst.Bounds(), padColor, image.Point{}, draw.Src)
	}
	// Centered, so a padded screenshot is not stuck against one edge.
	at := image.Rect((canvasW-w)/2, (canvasH-h)/2, (canvasW-w)/2+w, (canvasH-h)/2+h)
	xdraw.CatmullRom.Scale(dst, at, src, src.Bounds(), xdraw.Over, nil)

	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, fmt.Errorf("encode image: %w", err)
	}
	return buf.Bytes(), nil
}

// previewSize returns the size the screenshot itself is scaled to, preserving
// its aspect ratio: capped at maxDim on the longest side, then enlarged towards
// MinPreviewDim by at most maxPreviewUpscale and never past maxDim. A short
// side still below MinPreviewDim afterwards is the caller's to pad.
func previewSize(w, h, maxDim int) (int, int) {
	if maxDim > 0 && (w > maxDim || h > maxDim) {
		w, h = scaleToFit(w, h, maxDim)
	}
	short, long := min(w, h), max(w, h)
	if short >= MinPreviewDim {
		return w, h
	}

	// The enlargement as an exact ratio num/den, so the short side lands on
	// MinPreviewDim rather than one pixel below it.
	num, den := MinPreviewDim, short
	if num > den*maxPreviewUpscale {
		num, den = maxPreviewUpscale, 1
	}
	if maxDim > 0 && long*num/den > maxDim {
		num, den = maxDim, long
	}
	if num <= den {
		return w, h
	}
	return max(1, w*num/den), max(1, h*num/den)
}

// scaleToFit returns w and h scaled down so the longest side is exactly maxDim,
// preserving aspect ratio. w or h is assumed to already exceed maxDim.
func scaleToFit(w, h, maxDim int) (int, int) {
	if w >= h {
		return maxDim, max(1, h*maxDim/w)
	}
	return max(1, w*maxDim/h), maxDim
}
