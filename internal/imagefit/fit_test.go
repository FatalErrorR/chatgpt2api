package imagefit

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"testing"
)

func noisyPNG(w, h int, alpha bool) []byte {
	rnd := rand.New(rand.NewSource(int64(w*h + 1)))
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a := uint8(255)
			if alpha && x < w/4 {
				a = 0
			}
			img.SetNRGBA(x, y, color.NRGBA{uint8(rnd.Intn(256)), uint8(rnd.Intn(256)), uint8(x % 256), a})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func TestUnderBudgetReturnsOriginals(t *testing.T) {
	images := [][]byte{noisyPNG(64, 64, false)}
	got := FitReferenceImages(images, len(images[0])+1, DefaultMaxEdge)
	if !bytes.Equal(got[0], images[0]) {
		t.Fatal("an image under budget must not change")
	}
}

func TestOverBudgetFitsThePackage(t *testing.T) {
	images := [][]byte{noisyPNG(900, 700, false), noisyPNG(600, 600, true), noisyPNG(300, 900, false)}
	budget := 400 * 1024
	if total(images) <= budget {
		t.Fatalf("test images too small: %d", total(images))
	}
	got := FitReferenceImages(images, budget, DefaultMaxEdge)
	if total(got) > budget {
		t.Fatalf("package %d over budget %d", total(got), budget)
	}
	for i, item := range got {
		if _, _, err := image.Decode(bytes.NewReader(item)); err != nil {
			t.Fatalf("image %d unreadable after fitting: %v", i, err)
		}
	}
}

func TestLongEdgeIsCapped(t *testing.T) {
	images := [][]byte{noisyPNG(3000, 1000, false)}
	got := FitReferenceImages(images, len(images[0])-1, 2048)
	config, _, err := image.DecodeConfig(bytes.NewReader(got[0]))
	if err != nil || max(config.Width, config.Height) > 2048 {
		t.Fatalf("config %+v err %v", config, err)
	}
}

func TestUnreadableImagesAreLeftAlone(t *testing.T) {
	junk := bytes.Repeat([]byte("not an image"), 100)
	got := FitReferenceImages([][]byte{junk}, 10, DefaultMaxEdge)
	if !bytes.Equal(got[0], junk) {
		t.Fatal("unreadable image changed")
	}
}

func TestLeftoverBudget(t *testing.T) {
	if got := LeftoverImageBudget("prompt", 10000, 2); got != 10000-(6+4096+512) {
		t.Fatalf("got %d", got)
	}
	if LeftoverImageBudget("x", 10, 1) != 1 {
		t.Fatal("never below 1")
	}
}

// jpegWithOrientation writes a w x h JPEG whose EXIF says "rotate 90 clockwise" (6).
func jpegWithOrientation(w, h, orientation int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, 0, color.RGBA{255, 0, 0, 255})
	}
	var plain bytes.Buffer
	jpeg.Encode(&plain, img, &jpeg.Options{Quality: 90})
	tiff := []byte("II*\x00\x08\x00\x00\x00\x01\x00")
	entry := make([]byte, 12)
	binary.LittleEndian.PutUint16(entry[0:], 0x0112)
	binary.LittleEndian.PutUint16(entry[2:], 3)
	binary.LittleEndian.PutUint32(entry[4:], 1)
	binary.LittleEndian.PutUint16(entry[8:], uint16(orientation))
	tiff = append(append(tiff, entry...), 0, 0, 0, 0)
	app1 := append([]byte("Exif\x00\x00"), tiff...)
	segment := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(segment[2:], uint16(len(app1)+2))
	data := plain.Bytes()
	return append(append(append([]byte{}, data[:2]...), append(segment, app1...)...), data[2:]...)
}

func TestExifOrientationIsApplied(t *testing.T) {
	data := jpegWithOrientation(40, 10, 6)
	if got := jpegOrientation(data); got != 6 {
		t.Fatalf("orientation %d", got)
	}
	d := open(data)
	if d.img.Bounds().Dx() != 10 || d.img.Bounds().Dy() != 40 {
		t.Fatalf("not rotated: %v", d.img.Bounds())
	}
	if jpegOrientation(noisyPNG(4, 4, false)) != 1 {
		t.Fatal("non-JPEG has no orientation")
	}
}
