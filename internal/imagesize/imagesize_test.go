package imagesize

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// Ported from test/test_image_upscale.py (the Lanczos and exact-size parts).

func pngOf(w, h int, alpha bool) []byte {
	var img image.Image
	if alpha {
		rgba := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				rgba.SetNRGBA(x, y, color.NRGBA{200, 80, 40, uint8(x % 256)})
			}
		}
		img = rgba
	} else {
		rgb := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				rgb.Set(x, y, color.RGBA{200, 80, 40, 255})
			}
		}
		img = rgb
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func sizeOf(t *testing.T, data []byte) (int, int) {
	t.Helper()
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return config.Width, config.Height
}

func same(a, b []byte) bool { return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0]) }

func TestUpscaleKeepsRatio(t *testing.T) {
	if w, h := sizeOf(t, UpscaleToEdge(pngOf(1536, 1024, false), 2048)); w != 2048 || h != 1365 {
		t.Fatalf("landscape %dx%d", w, h)
	}
	if w, h := sizeOf(t, UpscaleToEdge(pngOf(1086, 1448, false), 2048)); w != 1536 || h != 2048 {
		t.Fatalf("portrait %dx%d", w, h)
	}
}

func TestUpscaleSkipsLargeAndExcessive(t *testing.T) {
	large := pngOf(2048, 1024, false)
	if !same(UpscaleToEdge(large, 2048), large) {
		t.Fatal("already large must be untouched")
	}
	tiny := pngOf(256, 256, false)
	if !same(UpscaleToEdge(tiny, 2048), tiny) {
		t.Fatal("more than 4x must be skipped")
	}
}

func TestUpscaleKeepsAlpha(t *testing.T) {
	out := UpscaleToEdge(pngOf(1024, 768, true), 2048)
	img, _, _ := image.Decode(bytes.NewReader(out))
	if img.Bounds().Dx() != 2048 || img.Bounds().Dy() != 1536 {
		t.Fatal(img.Bounds())
	}
	if _, _, _, a := img.At(10, 10).RGBA(); a == 0xffff {
		t.Fatal("alpha lost")
	}
}

func TestRequestedLongEdge(t *testing.T) {
	for in, want := range map[string]int{"1024x1024": 1024, "1536x1024": 1536, "1024X1536": 1536, "2048 × 1152": 2048, "2K": 2048, "4k": 4096, "": 0, "auto": 0, "square": 0, "16:9": 0} {
		if got := RequestedLongEdge(in); got != want {
			t.Errorf("%q: %d want %d", in, got, want)
		}
	}
}

func TestRequestedDimensions(t *testing.T) {
	if w, h, ok := RequestedDimensions("1536 × 1024"); !ok || w != 1536 || h != 1024 {
		t.Fatal("1536 × 1024")
	}
	for _, in := range []string{"", "auto", "2K", "16:9", "8000x8000", "10x10"} {
		if _, _, ok := RequestedDimensions(in); ok {
			t.Errorf("%q should not parse", in)
		}
	}
}

func TestFitToSize(t *testing.T) {
	if w, h := sizeOf(t, FitToSize(pngOf(1254, 1254, false), 1024, 1024, true)); w != 1024 || h != 1024 {
		t.Fatal("scale down")
	}
	exact := pngOf(1024, 1024, false)
	if !same(FitToSize(exact, 1024, 1024, true), exact) {
		t.Fatal("exact must be untouched")
	}
	if w, h := sizeOf(t, FitToSize(pngOf(1254, 1254, false), 2048, 2048, true)); w != 2048 || h != 2048 {
		t.Fatal("scale up")
	}
	if w, h := sizeOf(t, FitToSize(pngOf(1086, 1448, false), 1024, 1024, true)); w != 1024 || h != 1024 {
		t.Fatal("crop square")
	}
	if w, h := sizeOf(t, FitToSize(pngOf(1448, 1086, false), 1024, 1536, true)); w != 1024 || h != 1536 {
		t.Fatal("crop portrait")
	}
	square := pngOf(1254, 1254, false)
	if !same(FitToSize(square, 2048, 2048, false), square) {
		t.Fatal("upscale off, same ratio: untouched")
	}
	if w, h := sizeOf(t, FitToSize(pngOf(1086, 1448, false), 2048, 2048, false)); w != 1086 || h != 1086 {
		t.Fatalf("upscale off: crop only, got %dx%d", w, h)
	}
	tiny := pngOf(256, 256, false)
	if !same(FitToSize(tiny, 2048, 2048, true), tiny) {
		t.Fatal("excessive upscale skipped")
	}
}

func TestCropKeepsTheCenter(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 100, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 100; x++ {
			c := color.RGBA{255, 0, 0, 255}
			if y >= 75 && y < 125 {
				c = color.RGBA{0, 255, 0, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	out, _, _ := image.Decode(bytes.NewReader(FitToSize(buf.Bytes(), 100, 100, true)))
	if r, g, _, _ := out.At(50, 50).RGBA(); r != 0 || g != 0xffff {
		t.Fatal("centre not green")
	}
	if r, g, _, _ := out.At(50, 2).RGBA(); r != 0xffff || g != 0 {
		t.Fatal("top not red")
	}
}

func TestMaybeUpscalePipeline(t *testing.T) {
	on := Settings{ExactSize: true, Upscale: true, UpscaleToEdge: 2048}
	data := pngOf(1024, 1024, false)
	if !same(MaybeUpscale(data, "1024x1024", on), data) {
		t.Fatal("1024 request stays 1024")
	}
	upscaleOnly := Settings{Upscale: true, UpscaleToEdge: 2048}
	if w, _ := sizeOf(t, MaybeUpscale(pngOf(1024, 683, false), "1536x1024", upscaleOnly)); w != 1536 {
		t.Fatalf("raised only to the requested edge: %d", w)
	}
	if w, h := sizeOf(t, MaybeUpscale(pngOf(1024, 1024, false), "4K", upscaleOnly)); w != 2048 || h != 2048 {
		t.Fatal("4K capped by the configured target")
	}
	for _, size := range []string{"", "auto"} {
		if w, h := sizeOf(t, MaybeUpscale(pngOf(1024, 1024, false), size, upscaleOnly)); w != 2048 || h != 2048 {
			t.Fatalf("%q uses the configured target", size)
		}
	}
	exactOff := Settings{Upscale: false, ExactSize: true}
	if w, h := sizeOf(t, MaybeUpscale(pngOf(1254, 1254, false), "1024x1024", exactOff)); w != 1024 || h != 1024 {
		t.Fatal("exact size works with upscale off")
	}
	junk := []byte("not an image")
	if !same(MaybeUpscale(junk, "1024x1024", on), junk) {
		t.Fatal("failure returns the original")
	}
}
