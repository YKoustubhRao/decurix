package engine

import (
	"bytes"
	"image"
	"image/color"
	_ "image/gif" // decoders
	"image/jpeg"
	"image/png"
)

// shrink returns a picture no larger than maxSide on its longer side, as PNG
// (or JPEG for a JPEG), using a box filter so text stays legible. Pictures it
// cannot decode, or already small enough, come back unchanged. On a CPU every
// pixel shown to a vision model is prompt it has to read; OCR always gets the
// original.
func shrink(data []byte, maxSide int) []byte {
	if maxSide <= 0 {
		return data
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (cfg.Width <= maxSide && cfg.Height <= maxSide) {
		return data
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data
	}
	w, h := cfg.Width, cfg.Height
	nw, nh := maxSide, h*maxSide/w
	if h > w {
		nw, nh = w*maxSide/h, maxSide
	}
	if nw < 1 || nh < 1 {
		return data
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	b := src.Bounds()
	for y := 0; y < nh; y++ {
		y0, y1 := b.Min.Y+y*h/nh, b.Min.Y+(y+1)*h/nh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < nw; x++ {
			x0, x1 := b.Min.X+x*w/nw, b.Min.X+(x+1)*w/nw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, a, n uint64
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					cr, cg, cb, ca := src.At(xx, yy).RGBA()
					r, g, bl, a, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca), n+1
				}
			}
			dst.Set(x, y, color.RGBA64{uint16(r / n), uint16(g / n), uint16(bl / n), uint16(a / n)})
		}
	}
	var out bytes.Buffer
	if format == "jpeg" {
		err = jpeg.Encode(&out, dst, &jpeg.Options{Quality: 90})
	} else {
		err = png.Encode(&out, dst)
	}
	if err != nil {
		return data
	}
	return out.Bytes()
}
