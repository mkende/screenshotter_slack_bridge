package imageproc

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func dims(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height
}

func TestMaybeResizeDisabled(t *testing.T) {
	in := makePNG(t, 100, 50)
	out, err := MaybeResize(in, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(in, out) {
		t.Error("maxDim=0 should return the original bytes unchanged")
	}
}

func TestMaybeResizeUnderCap(t *testing.T) {
	in := makePNG(t, 100, 50)
	out, err := MaybeResize(in, 200)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(in, out) {
		t.Error("image under cap should be returned unchanged")
	}
}

func TestMaybeResizeDownscalesLandscape(t *testing.T) {
	in := makePNG(t, 1000, 500)
	out, err := MaybeResize(in, 200)
	if err != nil {
		t.Fatal(err)
	}
	w, h := dims(t, out)
	if w != 200 || h != 100 {
		t.Errorf("expected 200x100, got %dx%d", w, h)
	}
}

func TestMaybeResizeDownscalesPortrait(t *testing.T) {
	in := makePNG(t, 500, 1000)
	out, err := MaybeResize(in, 200)
	if err != nil {
		t.Fatal(err)
	}
	w, h := dims(t, out)
	if w != 100 || h != 200 {
		t.Errorf("expected 100x200, got %dx%d", w, h)
	}
}

func TestMaybeResizeRejectsBomb(t *testing.T) {
	// A header claiming huge dimensions should be rejected without decoding.
	big := makePNG(t, 1, 1)
	// Corrupt the IHDR to claim an enormous size is overkill; instead make a
	// genuinely oversized small allocation by checking the guard directly with
	// a real but large header is impractical, so we trust the guard via a
	// dimension just over the pixel cap.
	_ = big
	in := makePNG(t, 7000, 7000) // 49 Mpx > 40 Mpx cap
	if _, err := MaybeResize(in, 0); err == nil {
		t.Error("expected error for image exceeding pixel cap")
	}
}
