package gateway

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"sync"
)

// iconPNG is the page's icon for a phone's home screen, which takes no SVG:
// three agents' states, each a dot and a line. It is drawn on first use, not
// at start: the same binary runs every agent hook.
var iconPNG = sync.OnceValue(func() []byte {
	const size, samples = 180, 4
	ink := color.RGBA{0x14, 0x17, 0x1c, 0xff}
	line := color.RGBA{0xd8, 0xda, 0xdd, 0xff}
	dots := []color.RGBA{{0xf0, 0x78, 0x4f, 0xff}, {0x6d, 0x9c, 0xf0, 0xff}, {0x5c, 0xc0, 0x8a, 0xff}} // blocked, working, idle
	lengths := []float64{72, 96, 56}

	type shape struct {
		inside func(x, y float64) bool
		c      color.RGBA
	}
	var shapes []shape
	for i := range 3 {
		cy := 54 + float64(i)*36
		dot := dots[i]
		shapes = append(shapes, shape{func(x, y float64) bool { return math.Hypot(x-46, y-cy) <= 11 }, dot})
		x0, x1 := 72.0, 72+lengths[i]
		shapes = append(shapes, shape{func(x, y float64) bool {
			// A bar with round ends, 12 high.
			px := math.Max(x0+6, math.Min(x, x1-6))
			return math.Hypot(x-px, y-cy) <= 6
		}, line})
	}

	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := range size {
		for x := range size {
			var r, g, b float64
			for sy := range samples {
				for sx := range samples {
					fx, fy := float64(x)+(float64(sx)+0.5)/samples, float64(y)+(float64(sy)+0.5)/samples
					c := ink
					for _, s := range shapes {
						if s.inside(fx, fy) {
							c = s.c
						}
					}
					r, g, b = r+float64(c.R), g+float64(c.G), b+float64(c.B)
				}
			}
			n := float64(samples * samples)
			img.SetRGBA(x, y, color.RGBA{uint8(r / n), uint8(g / n), uint8(b / n), 0xff})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
})
