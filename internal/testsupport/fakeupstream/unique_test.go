package fakeupstream

import (
	"bytes"
	"image/png"
	"testing"
)

func TestUniqueImagesAreValidAndDifferent(t *testing.T) {
	base := samplePNG()
	a, b := withTextChunk(base, "file-1.png"), withTextChunk(base, "file-2.png")
	if bytes.Equal(a, b) || bytes.Equal(a, base) {
		t.Fatal("images are not different")
	}
	for _, raw := range [][]byte{a, b} {
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil || img.Bounds().Dx() != 256 {
			t.Fatalf("not a valid 256 px PNG: %v", err)
		}
	}
}
