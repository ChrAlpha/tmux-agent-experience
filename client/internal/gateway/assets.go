package gateway

import (
	"bytes"
	"embed"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"path"
	"strconv"
	"strings"
)

//go:embed assets
var assets embed.FS

var contentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".webmanifest": "application/manifest+json",
}

// asset serves the page's files. The page is one document whose scripts
// come from this origin alone.
func (g *Gateway) asset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	var b []byte
	if icon, ok := g.icons[name]; ok {
		b = icon
	} else {
		var err error
		if b, err = assets.ReadFile("assets/" + name); err != nil {
			http.NotFound(w, r)
			return
		}
	}
	h := w.Header()
	h.Set("Content-Type", contentTypes[path.Ext(name)])
	if name == "index.html" {
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	}
	if name == "sw.js" {
		h.Set("Service-Worker-Allowed", "/")
	}
	_, _ = w.Write(b)
}

// icons draws the home-screen icons, which iOS takes only as PNG.
func icons() map[string][]byte {
	out := map[string][]byte{}
	for _, size := range []int{180, 512} {
		var buf bytes.Buffer
		_ = png.Encode(&buf, drawIcon(size))
		out["icon-"+strconv.Itoa(size)+".png"] = buf.Bytes()
	}
	return out
}

// drawIcon is the mark of icon.svg: a prompt's chevron and a waiting dot on
// the page's dark ground, sampled four times per pixel on each axis.
func drawIcon(size int) image.Image {
	ground := color.NRGBA{0x16, 0x14, 0x12, 0xff}
	ink := color.NRGBA{0xf3, 0xee, 0xe7, 0xff}
	amber := color.NRGBA{0xf2, 0xa5, 0x3a, 0xff}
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const ss = 4
	for py := range size {
		for px := range size {
			var hits [3]int
			for sy := range ss {
				for sx := range ss {
					// In the 100-unit square of icon.svg.
					x := (float64(px) + (float64(sx)+0.5)/ss) * 100 / float64(size)
					y := (float64(py) + (float64(sy)+0.5)/ss) * 100 / float64(size)
					switch {
					case math.Hypot(x-70, y-66) <= 9:
						hits[2]++
					case segment(x, y, 26, 30, 48, 50) <= 6.5 || segment(x, y, 48, 50, 26, 70) <= 6.5:
						hits[1]++
					default:
						hits[0]++
					}
				}
			}
			img.SetNRGBA(px, py, mix([3]color.NRGBA{ground, ink, amber}, hits, ss*ss))
		}
	}
	return img
}

// segment is the distance from (x, y) to the segment from (ax, ay) to
// (bx, by).
func segment(x, y, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	t := math.Max(0, math.Min(1, ((x-ax)*dx+(y-ay)*dy)/(dx*dx+dy*dy)))
	return math.Hypot(x-ax-t*dx, y-ay-t*dy)
}

func mix(colors [3]color.NRGBA, weights [3]int, total int) color.NRGBA {
	var r, g, b int
	for i, c := range colors {
		r += int(c.R) * weights[i]
		g += int(c.G) * weights[i]
		b += int(c.B) * weights[i]
	}
	return color.NRGBA{uint8(r / total), uint8(g / total), uint8(b / total), 0xff}
}
