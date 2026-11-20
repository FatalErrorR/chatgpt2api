// Package imagefit is utils/image_fit.py: keep the picture_v2 reference images under the upload
// budget, changing as little as possible.
//
// Differences from the Python version (PIL): no WebP encoder in Go, so images with alpha try PNG
// before JPEG (Python tries WebP first; both end at JPEG when that is what fits); resizing uses
// Catmull-Rom instead of Lanczos. EXIF orientation is applied before re-encoding, as
// ImageOps.exif_transpose does.
package imagefit

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	DefaultBudgetBytes   = 1468006 // int(1.4 * 1024 * 1024)
	DefaultMaxEdge       = 2048
	requestOverheadBytes = 4096
	requestPerImageBytes = 256
	downscaleFactor      = 0.85
	maxShrinkRounds      = 24
	minJustFitBytes      = 80 * 1024
	smallestEdgeToShrink = 256
)

var jpegQualities = []int{90, 85, 80, 75, 70, 65, 60, 50, 40}

// LeftoverImageBudget is what the request budget leaves for images after the prompt and overhead.
func LeftoverImageBudget(prompt string, budget, images int) int {
	leftover := budget - (len(prompt) + requestOverheadBytes + requestPerImageBytes*max(images, 0))
	if leftover > 0 {
		return leftover
	}
	return 1
}

// Options are the image_ref_fit_* / image_req_fit_* settings.
type Options struct {
	RefEnabled bool
	RefBudget  int
	ReqEnabled bool
	ReqBudget  int
	MaxEdge    int
}

// FitWebReferenceImages fits the package once to the tighter of the file budget and the budget the
// request leaves for images.
func FitWebReferenceImages(images [][]byte, prompt string, o Options) [][]byte {
	if len(images) == 0 {
		return [][]byte{}
	}
	budgets := []int{}
	if o.RefEnabled {
		budgets = append(budgets, o.RefBudget)
	}
	if o.ReqEnabled {
		budgets = append(budgets, LeftoverImageBudget(prompt, o.ReqBudget, len(images)))
	}
	if len(budgets) == 0 {
		return append([][]byte(nil), images...)
	}
	budget := budgets[0]
	for _, b := range budgets[1:] {
		budget = min(budget, b)
	}
	return FitReferenceImages(images, budget, o.MaxEdge)
}

// FitReferenceImages returns the originals when they are under budget; otherwise shrinks.
// Unreadable images are left alone; a panic falls back to the originals.
func FitReferenceImages(images [][]byte, budget, maxEdge int) (out [][]byte) {
	if len(images) == 0 {
		return [][]byte{}
	}
	originals := append([][]byte(nil), images...)
	if total(originals) <= budget {
		return originals
	}
	defer func() {
		if recover() != nil {
			out = originals
		}
	}()
	return fit(originals, budget, maxEdge)
}

func total(items [][]byte) int {
	sum := 0
	for _, item := range items {
		sum += len(item)
	}
	return sum
}

type decoded struct {
	img    image.Image
	format string
}

func open(data []byte) *decoded {
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	if format == "jpeg" {
		img = applyOrientation(img, jpegOrientation(data))
	}
	return &decoded{img: img, format: format}
}

func hasAlpha(img image.Image) bool {
	switch img.(type) {
	case *image.NRGBA, *image.RGBA, *image.NRGBA64, *image.RGBA64:
	case *image.Paletted:
	default:
		return false
	}
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0xffff {
				return true
			}
		}
	}
	return false
}

// toRGB flattens alpha onto white (_to_rgb).
func toRGB(img image.Image) image.Image {
	bounds := img.Bounds()
	out := image.NewRGBA(bounds)
	draw.Draw(out, bounds, &image.Uniform{color.White}, image.Point{}, draw.Src)
	draw.Draw(out, bounds, img, bounds.Min, draw.Over)
	return out
}

func resize(img image.Image, width, height int) image.Image {
	out := image.NewNRGBA(image.Rect(0, 0, max(1, width), max(1, height)))
	xdraw.CatmullRom.Scale(out, out.Bounds(), img, img.Bounds(), draw.Src, nil)
	return out
}

func resizeToEdge(img image.Image, maxEdge int) image.Image {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	longest := max(w, h)
	if longest <= maxEdge {
		return img
	}
	scale := float64(maxEdge) / float64(longest)
	return resize(img, int(float64(w)*scale), int(float64(h)*scale))
}

func encodeJPEG(img image.Image, quality int) []byte {
	var buf bytes.Buffer
	jpeg.Encode(&buf, toRGB(img), &jpeg.Options{Quality: quality})
	return buf.Bytes()
}

func encodePNG(img image.Image) []byte {
	var buf bytes.Buffer
	(&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, img)
	return buf.Bytes()
}

func smallest(items ...[]byte) []byte {
	best := items[0]
	for _, item := range items[1:] {
		if len(item) < len(best) {
			best = item
		}
	}
	return best
}

func encodeAfterResize(img image.Image, format string) []byte {
	jpg := encodeJPEG(img, 90)
	if format == "jpeg" {
		return jpg
	}
	if hasAlpha(img) || format == "png" || format == "webp" {
		return smallest(encodePNG(img), jpg)
	}
	return jpg
}

func candidates(img image.Image) [][]byte {
	out := [][]byte{}
	if hasAlpha(img) {
		out = append(out, encodePNG(img))
	}
	for _, quality := range jpegQualities {
		out = append(out, encodeJPEG(img, quality))
	}
	return out
}

func encodeToFit(img image.Image, limit int) []byte {
	for _, candidate := range candidates(img) {
		if len(candidate) <= limit {
			return candidate
		}
	}
	return nil
}

func largestUnder(img image.Image, limit int) []byte {
	var best []byte
	for _, candidate := range candidates(img) {
		if len(candidate) > 0 && len(candidate) <= limit && len(candidate) > len(best) {
			best = candidate
		}
	}
	return best
}

// oneStepSmaller is _one_step_smaller: about 15% smaller, keeping the largest file that shrinks.
func oneStepSmaller(data []byte, maxEdge int) []byte {
	d := open(data)
	if d == nil {
		return data
	}
	current := resizeToEdge(d.img, maxEdge)
	if stepped := largestUnder(current, max(1, int(float64(len(data))*downscaleFactor))); stepped != nil {
		return stepped
	}
	w, h := current.Bounds().Dx(), current.Bounds().Dy()
	if max(w, h) > smallestEdgeToShrink {
		current = resize(current, int(float64(w)*downscaleFactor), int(float64(h)*downscaleFactor))
	}
	if fallback := largestUnder(current, len(data)-1); fallback != nil {
		return fallback
	}
	return data
}

// shrinkOne is _shrink_one: the best encoding under limit, shrinking 15% a round when needed.
func shrinkOne(data []byte, limit, maxEdge int) []byte {
	d := open(data)
	if d == nil {
		return data
	}
	current := resizeToEdge(d.img, maxEdge)
	if fitted := encodeToFit(current, limit); fitted != nil {
		if len(fitted) < len(data) {
			return fitted
		}
		return data
	}
	for round := 0; round < maxShrinkRounds; round++ {
		w, h := current.Bounds().Dx(), current.Bounds().Dy()
		if max(w, h) <= smallestEdgeToShrink {
			break
		}
		current = resize(current, int(float64(w)*downscaleFactor), int(float64(h)*downscaleFactor))
		if fitted := encodeToFit(current, limit); fitted != nil {
			return fitted
		}
	}
	if last := encodeJPEG(current, jpegQualities[len(jpegQualities)-1]); len(last) < len(data) {
		return last
	}
	return data
}

func fit(images [][]byte, budget, maxEdge int) [][]byte {
	result := append([][]byte(nil), images...)
	skip := make([]bool, len(result))
	for i, item := range result {
		d := open(item)
		if d == nil {
			skip[i] = true
			continue
		}
		if max(d.img.Bounds().Dx(), d.img.Bounds().Dy()) <= maxEdge {
			continue
		}
		if encoded := encodeAfterResize(resizeToEdge(d.img, maxEdge), d.format); len(encoded) < len(result[i]) {
			result[i] = encoded
		}
	}
	rounds := max(64, len(result)*(len(jpegQualities)+maxShrinkRounds))
	for round := 0; round < rounds; round++ {
		sum := total(result)
		if sum <= budget {
			return result
		}
		index := -1
		for i := range result {
			if !skip[i] && (index < 0 || len(result[i]) > len(result[index])) {
				index = i
			}
		}
		if index < 0 {
			break
		}
		others := sum - len(result[index])
		leftover := budget - others
		current := len(result[index])
		var shrunk []byte
		if others <= budget && leftover >= min(minJustFitBytes, max(1, current/2)) {
			shrunk = shrinkOne(result[index], leftover, maxEdge)
		} else {
			shrunk = oneStepSmaller(result[index], maxEdge)
		}
		if len(shrunk) >= len(result[index]) {
			skip[index] = true
			continue
		}
		result[index] = shrunk
	}
	return result
}
