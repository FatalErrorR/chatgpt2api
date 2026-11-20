// Package imagesize is utils/image_upscale.py and maybe_upscale_image: crop and resize a returned
// image to the requested WxH (image_exact_size_enabled), or enlarge it to a target long edge
// (image_upscale_enabled). Any failure returns the original bytes.
//
// The "model" method (RealESRGAN via ONNX) is not available in Go; it uses the Lanczos path, which
// is also what the Python version falls back to when the model fails.
package imagesize

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"math"
	"regexp"
	"strconv"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	maxScale        = 4.0
	minExactEdge    = 64
	maxExactEdge    = 4096
	aspectTolerance = 0.02
)

var (
	sizeWxH = regexp.MustCompile(`(?i)(\d{2,5})\s*[x×*]\s*(\d{2,5})`)
	sizeK   = regexp.MustCompile(`(?i)^\s*([1-8])\s*k\s*$`)
)

// Settings are the image_upscale_* / image_exact_size_enabled values.
type Settings struct {
	ExactSize     bool
	Upscale       bool
	UpscaleMethod string
	UpscaleToEdge int
}

// RequestedLongEdge parses "1536x1024" or "2K" to the long edge; 0 when unknown.
func RequestedLongEdge(size string) int {
	if match := sizeWxH.FindStringSubmatch(size); match != nil {
		w, _ := strconv.Atoi(match[1])
		h, _ := strconv.Atoi(match[2])
		return max(w, h)
	}
	if match := sizeK.FindStringSubmatch(size); match != nil {
		k, _ := strconv.Atoi(match[1])
		return k * 1024
	}
	return 0
}

// RequestedDimensions parses an explicit WxH within 64..4096.
func RequestedDimensions(size string) (int, int, bool) {
	match := sizeWxH.FindStringSubmatch(size)
	if match == nil {
		return 0, 0, false
	}
	w, _ := strconv.Atoi(match[1])
	h, _ := strconv.Atoi(match[2])
	for _, v := range []int{w, h} {
		if v < minExactEdge || v > maxExactEdge {
			return 0, 0, false
		}
	}
	return w, h, true
}

// pyRound is Python's round(): halves go to the even neighbour.
func pyRound(v float64) int { return int(math.RoundToEven(v)) }

// MaybeUpscale is maybe_upscale_image.
func MaybeUpscale(data []byte, size string, s Settings) (out []byte) {
	defer func() {
		if recover() != nil {
			out = data
		}
	}()
	if s.ExactSize {
		if w, h, ok := RequestedDimensions(size); ok {
			return FitToSize(data, w, h, s.Upscale)
		}
	}
	if !s.Upscale {
		return data
	}
	target := s.UpscaleToEdge
	if requested := RequestedLongEdge(size); requested > 0 {
		target = min(target, requested)
	}
	return UpscaleToEdge(data, target)
}

func decode(data []byte) (image.Image, bool) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	bounds := img.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(out, out.Bounds(), img, bounds.Min, draw.Src)
	return out, true
}

func encodePNG(img image.Image) []byte {
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func resizeHigh(img image.Image, w, h int) *image.NRGBA {
	out := image.NewNRGBA(image.Rect(0, 0, max(1, w), max(1, h)))
	xdraw.CatmullRom.Scale(out, out.Bounds(), img, img.Bounds(), draw.Src, nil)
	return out
}

// enlarge is _enlarge_to_png with the Lanczos path: resize, then a light unsharp mask.
func enlarge(img image.Image, w, h int) []byte {
	return encodePNG(unsharpMask(resizeHigh(img, w, h), 1.2, 60, 2))
}

// UpscaleToEdge enlarges (never shrinks) to the target long edge, at most 4x; output is PNG.
func UpscaleToEdge(data []byte, target int) []byte {
	img, ok := decode(data)
	if !ok {
		return data
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	long := max(w, h)
	if long <= 0 || long >= target {
		return data
	}
	scale := float64(target) / float64(long)
	if scale > maxScale {
		return data
	}
	return enlarge(img, max(1, pyRound(float64(w)*scale)), max(1, pyRound(float64(h)*scale)))
}

// FitToSize is fit_to_size: centre-crop to the requested ratio (beyond 2% off), then resize to
// exactly WxH; enlarging only when allowed and at most 4x. Already exact: unchanged.
func FitToSize(data []byte, width, height int, allowUpscale bool) []byte {
	img, ok := decode(data)
	if !ok {
		return data
	}
	srcW, srcH := img.Bounds().Dx(), img.Bounds().Dy()
	if srcW <= 0 || srcH <= 0 || (srcW == width && srcH == height) {
		return data
	}
	wanted := float64(width) / float64(height)
	current := image.Image(img)
	curW, curH := srcW, srcH
	if math.Abs(float64(srcW)/float64(srcH)/wanted-1) > aspectTolerance {
		newW, newH := srcW, srcH
		if float64(srcW)/float64(srcH) > wanted {
			newW = max(1, pyRound(float64(srcH)*wanted))
		} else {
			newH = max(1, pyRound(float64(srcW)/wanted))
		}
		left, top := (srcW-newW)/2, (srcH-newH)/2
		cropped := image.NewNRGBA(image.Rect(0, 0, newW, newH))
		draw.Draw(cropped, cropped.Bounds(), img, image.Point{left, top}, draw.Src)
		current, curW, curH = cropped, newW, newH
	}
	scale := math.Max(float64(width)/float64(curW), float64(height)/float64(curH))
	if scale > 1 && allowUpscale && scale <= maxScale {
		return enlarge(current, width, height)
	}
	if scale > 1 {
		if curW == srcW && curH == srcH {
			return data
		}
		return encodePNG(current)
	}
	return encodePNG(resizeHigh(current, width, height))
}

// unsharpMask is PIL's ImageFilter.UnsharpMask(radius, percent, threshold) on RGB, alpha kept.
func unsharpMask(img *image.NRGBA, radius float64, percent, threshold int) *image.NRGBA {
	blurred := gaussianBlur(img, radius)
	out := image.NewNRGBA(img.Bounds())
	for i := 0; i < len(img.Pix); i += 4 {
		for c := 0; c < 3; c++ {
			orig := int(img.Pix[i+c])
			diff := orig - int(blurred.Pix[i+c])
			if diff < 0 && -diff < threshold || diff >= 0 && diff < threshold {
				out.Pix[i+c] = img.Pix[i+c]
				continue
			}
			value := orig + diff*percent/100
			out.Pix[i+c] = uint8(max(0, min(255, value)))
		}
		out.Pix[i+3] = img.Pix[i+3]
	}
	return out
}

func gaussianBlur(img *image.NRGBA, sigma float64) *image.NRGBA {
	radius := int(math.Ceil(sigma * 3))
	kernel := make([]float64, 2*radius+1)
	sum := 0.0
	for i := range kernel {
		x := float64(i - radius)
		kernel[i] = math.Exp(-x * x / (2 * sigma * sigma))
		sum += kernel[i]
	}
	for i := range kernel {
		kernel[i] /= sum
	}
	b := img.Bounds()
	tmp := image.NewNRGBA(b)
	out := image.NewNRGBA(b)
	pass := func(src, dst *image.NRGBA, horizontal bool) {
		for y := 0; y < b.Dy(); y++ {
			for x := 0; x < b.Dx(); x++ {
				var acc [4]float64
				for k, weight := range kernel {
					sx, sy := x, y
					if horizontal {
						sx = min(b.Dx()-1, max(0, x+k-radius))
					} else {
						sy = min(b.Dy()-1, max(0, y+k-radius))
					}
					p := src.NRGBAAt(sx, sy)
					acc[0] += float64(p.R) * weight
					acc[1] += float64(p.G) * weight
					acc[2] += float64(p.B) * weight
					acc[3] += float64(p.A) * weight
				}
				dst.SetNRGBA(x, y, color.NRGBA{uint8(acc[0] + 0.5), uint8(acc[1] + 0.5), uint8(acc[2] + 0.5), uint8(acc[3] + 0.5)})
			}
		}
	}
	pass(img, tmp, true)
	pass(tmp, out, false)
	return out
}
