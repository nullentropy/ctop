// Command icon draws ctop's app icon and writes it as a PNG for
// `go tool appbundle -icon`.
//
//	go run ./tools/icon -o icon.png
//
// It draws the same things the app does at poster size: the phosphor
// dot-matrix chart, the green/amber/red ramp, a faint grid and a vignette.
package main

import (
	"flag"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
)

const (
	size = 1024 // final edge length
	ss   = 3    // supersampling factor; the whole thing is drawn at size*ss
	// macOS art sits inside a rounded square inset from the canvas edge, so
	// the shadow the OS adds has somewhere to fall.
	inset  = 64.0
	corner = 200.0
)

// The bars. Eleven columns, because eleven reads as a trace rather than as a
// logo, and it rises to a peak so the ramp shows all three of its colours.
var bars = []float64{0.30, 0.42, 0.34, 0.52, 0.46, 0.63, 0.75, 0.68, 0.88, 0.97, 0.72}

func main() {
	out := flag.String("o", "icon.png", "output PNG path")
	flag.Parse()

	big := size * ss
	img := image.NewNRGBA(image.Rect(0, 0, big, big))
	for y := range big {
		for x := range big {
			img.SetNRGBA(x, y, pixel(float64(x)/ss, float64(y)/ss))
		}
	}

	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, downsample(img)); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s (%dx%d)", *out, size, size)
}

// pixel shades one point in icon space (0..size), returning straight alpha.
func pixel(x, y float64) color.NRGBA {
	// Outside the rounded square: fully transparent.
	d := roundedRectSDF(x, y, inset, inset, size-inset, size-inset, corner)
	if d > 1 {
		return color.NRGBA{}
	}
	alpha := clamp(0.5-d, 0, 1)

	// Plate: a dark vertical gradient, lifted slightly at the top.
	t := (y - inset) / (size - 2*inset)
	r, g, b := 0.020+0.010*(1-t), 0.055+0.030*(1-t), 0.040+0.020*(1-t)

	inner := inset + 96 // the chart's own margin inside the plate
	span := size - 2*inner
	cell := span / float64(len(bars))

	// Faint grid, on the same pitch as the dots.
	gx := math.Abs(math.Mod(x-inner, cell)/cell - 0.5)
	gy := math.Abs(math.Mod(y-inner, cell)/cell - 0.5)
	if x > inner-cell && x < size-inner+cell && y > inner-cell && y < size-inner+cell {
		grid := smoothstep(0.47, 0.5, math.Max(gx, gy)) * 0.025
		r += grid * 0.2
		g += grid
		b += grid * 0.5
	}

	// The dot matrix.
	if x >= inner && x < size-inner && y >= inner && y < size-inner {
		col := int((x - inner) / cell)
		row := int((y - inner) / cell)
		rows := len(bars)
		if col >= 0 && col < len(bars) {
			lvl := float64(rows-1-row) / float64(rows)
			amt := clamp((bars[col]-lvl)*float64(rows), 0, 1)

			cx := inner + (float64(col)+0.5)*cell
			cy := inner + (float64(row)+0.5)*cell
			dist := math.Hypot(x-cx, y-cy) / cell
			// A tight falloff: wide antialiasing at this scale reads as blur.
			dot := 1 - smoothstep(0.31, 0.375, dist)

			lr, lg, lb := ramp(lvl)
			// Lit dots, plus an unlit field so the chart reads as a display.
			r += lr*amt*dot + 0.045*(1-amt)*dot
			g += lg*amt*dot + 0.125*(1-amt)*dot
			b += lb*amt*dot + 0.080*(1-amt)*dot

			// Bloom around the crest of each column.
			crest := math.Exp(-math.Abs(lvl-bars[col]) * float64(rows) * 0.8)
			r += lr * crest * 0.10
			g += lg * crest * 0.10
			b += lb * crest * 0.10
		}
	}

	// Scanlines and a vignette, the same two touches the CRT pass applies.
	r *= 0.93 + 0.07*math.Sin(y*math.Pi/3)
	g *= 0.93 + 0.07*math.Sin(y*math.Pi/3)
	b *= 0.93 + 0.07*math.Sin(y*math.Pi/3)
	nx, ny := (x-size/2)/(size/2), (y-size/2)/(size/2)
	v := 1 - 0.35*(nx*nx+ny*ny)
	r, g, b = r*v, g*v, b*v

	return color.NRGBA{
		R: uint8(clamp(r, 0, 1)*255 + 0.5),
		G: uint8(clamp(g, 0, 1)*255 + 0.5),
		B: uint8(clamp(b, 0, 1)*255 + 0.5),
		A: uint8(alpha*255 + 0.5),
	}
}

// ramp is the phosphor palette from shaders.go: green through amber to red.
func ramp(t float64) (r, g, b float64) {
	t = clamp(t, 0, 1)
	lo := [3]float64{0.16, 1.00, 0.55}
	mid := [3]float64{0.95, 0.90, 0.30}
	hi := [3]float64{1.00, 0.26, 0.36}
	if t < 0.55 {
		k := t / 0.55
		return lerp(lo[0], mid[0], k), lerp(lo[1], mid[1], k), lerp(lo[2], mid[2], k)
	}
	k := (t - 0.55) / 0.45
	return lerp(mid[0], hi[0], k), lerp(mid[1], hi[1], k), lerp(mid[2], hi[2], k)
}

// roundedRectSDF is the signed distance to a rounded rectangle: negative
// inside, positive outside, which gives an antialiased edge for free.
func roundedRectSDF(px, py, x0, y0, x1, y1, rad float64) float64 {
	cx := math.Max(math.Max(x0+rad-px, 0), px-(x1-rad))
	cy := math.Max(math.Max(y0+rad-py, 0), py-(y1-rad))
	return math.Hypot(cx, cy) - rad
}

func downsample(src *image.NRGBA) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := range size {
		for x := range size {
			var r, g, b, a int
			for dy := range ss {
				for dx := range ss {
					c := src.NRGBAAt(x*ss+dx, y*ss+dy)
					// Weight colour by coverage so edge pixels do not darken.
					r += int(c.R) * int(c.A)
					g += int(c.G) * int(c.A)
					b += int(c.B) * int(c.A)
					a += int(c.A)
				}
			}
			if a == 0 {
				continue
			}
			dst.SetNRGBA(x, y, color.NRGBA{
				R: uint8(r / a), G: uint8(g / a), B: uint8(b / a),
				A: uint8(a / (ss * ss)),
			})
		}
	}
	return dst
}

func clamp(v, lo, hi float64) float64 { return math.Min(math.Max(v, lo), hi) }
func lerp(a, b, t float64) float64    { return a + (b-a)*t }

func smoothstep(e0, e1, x float64) float64 {
	t := clamp((x-e0)/(e1-e0), 0, 1)
	return t * t * (3 - 2*t)
}
