package main

import (
	"math"

	caution "github.com/nullentropy/caution/go"
)

type pane struct {
	node *caution.Node
	u    map[string]float64
}

func (p *pane) flush() { p.node.SetUniforms(p.u) }

// set writes one uniform, sends just that key, and reports if it actually
// changed
func (p *pane) set(k string, v float64) bool {
	if p.u[k] == v {
		return false
	}
	p.u[k] = v
	p.node.SetUniform(k, v)
	return true
}

// SetPal switches the phosphor and repaints
func (p *pane) SetPal(i float64) { p.set("u_pal", i) }

// Node exposes the widget for tree building
func (p *pane) Node() *caution.Node { return p.node }

type Graph struct {
	pane
	raw    []float64 // ring contents, index 0 oldest, len == histSamples
	filled int       // how many slots hold a real sample
	max    float64   // fixed full-scale, or 0 for autoscaling
	peak   float64   // autoscale: decaying observed maximum
	floor  float64   // autoscale: smallest full-scale worth showing

	pin  int     // sample age the crosshair is pinned to, -1 for none
	pinW float64 // pane width the pin was last placed against
}

// NewGraph builds a history chart. cell is the dot pitch in px. fullScale
// fixes the vertical range (100 for a percentage). pass 0 to autoscale against
// a decaying peak, with floor as the smallest range it will zoom in to.
// stops an idle network graph amplifying noise to fill the pane.
func NewGraph(cell, fullScale, floor float64, samples int) *Graph {
	g := &Graph{
		raw:   make([]float64, samples),
		max:   fullScale,
		floor: floor,
	}
	g.pin = -1
	g.u = map[string]float64{"u_cell": cell, "u_fade": 1, "u_pal": themes[startTheme].Pal, "u_shift": 0, "u_probe": -1}
	for i := range g.raw {
		g.u[histUniform(i)] = 0
	}
	g.node = caution.Shader(caution.FX{Frag: graphFragFor(samples), Uniforms: g.u})
	return g
}

// Push appends one sample and pushes the new uniform set to the client. The
// scroll offset resets: the column that had been sliding into place is now
// the real newest column.
func (g *Graph) Push(v float64) {
	if v < 0 {
		v = 0
	}
	copy(g.raw, g.raw[1:])
	g.raw[len(g.raw)-1] = v
	if g.filled < len(g.raw) {
		g.filled++
	}

	scale := g.max
	if scale <= 0 {
		// autoscale: decay the peak so a one-off burst stops flattening the
		// graph a minute later, but never below the observed window maximum
		g.peak *= 0.94
		for _, s := range g.raw {
			if s > g.peak {
				g.peak = s
			}
		}
		scale = max(g.peak*1.15, g.floor)
	}

	start := len(g.raw) - g.filled
	for i := range g.raw {
		if i < start {
			g.u[histUniform(i)] = 0 // no data yet: unlit field
			continue
		}
		// A live sample of zero still earns one dot, so "idle" and "no data" don't look the same
		g.u[histUniform(i)] = min(max(g.raw[i]/scale, 0.012), 1)
	}
	if g.pin >= 0 {
		g.pin++ // the pinned sample just got one older, so its column moves
		g.u["u_probe"] = g.pinColumn()
	}
	g.u["u_shift"] = 0
	g.flush()
}

// Shift sets how far the chart has scrolled toward its next sample, 0..1.
func (g *Graph) Shift(f float64) bool {
	return g.set("u_shift", round3(min(max(f, 0), 0.999)))
}

// Pin fixes the scrub cursor to a sample age, placed against a pane w px wide.
// A negative age clears it.
func (g *Graph) Pin(back int, w float64) {
	if back < 0 {
		g.pin = -1
		g.set("u_probe", -1)
		return
	}
	g.pin, g.pinW = back, w
	g.set("u_probe", g.pinColumn())
}

// Pinned reports the pinned sample age, or -1.
func (g *Graph) Pinned() int { return g.pin }

// Len is the history depth.
func (g *Graph) Len() int { return len(g.raw) }

// pinColumn is where the pinned sample currently sits, in column indices.
func (g *Graph) pinColumn() float64 {
	return g.cols(g.pinW) - 1 - float64(g.pin)
}

// cols is how many columns the chart shows in a pane w px wide. This repeats
// the shader's own arithmetic, kept here so the two cannot disagree about
// which sample sits under a given pixel.
func (g *Graph) cols(w float64) float64 {
	cs := max(max(g.u["u_cell"], 2), w/float64(len(g.raw)))
	return max(math.Floor(w/cs), 1)
}

// ColumnAt maps a 0..1 fraction across a pane w px wide to the sample under it.
// back is how many samples old the hit is. ok is false over a column with no
// sample behind it.
func (g *Graph) ColumnAt(frac, w float64) (value float64, back int, ok bool) {
	if w <= 0 {
		return 0, 0, false
	}
	cols := g.cols(w)
	cell := math.Floor(min(max(frac, 0), 0.9999)*cols + g.u["u_shift"] - 1)
	back = int(cols - 1 - cell)
	return g.Sample(back)
}

// Sample reads a value by age in samples
func (g *Graph) Sample(back int) (value float64, n int, ok bool) {
	idx := len(g.raw) - 1 - back
	if back < 0 || idx < 0 || idx >= len(g.raw) || idx < len(g.raw)-g.filled {
		return 0, back, false
	}
	return g.raw[idx], back, true
}

// Scale reports the current full-scale value, so a caption can say what the
// top of an autoscaling graph means
func (g *Graph) Scale() float64 {
	if g.max > 0 {
		return g.max
	}
	return max(g.peak*1.15, g.floor)
}

// Cores is a bank of segmented LED meters, one per logical core. Values ease toward
// their targets so a core going from 5% to 90% sweeps rather than snaps.
type Cores struct {
	pane
	n      int
	target []float64
	cur    []float64
}

func NewCores(n int) *Cores {
	c := &Cores{n: min(n, coreSlots), target: make([]float64, coreSlots), cur: make([]float64, coreSlots)}
	c.u = map[string]float64{"u_n": float64(c.n), "u_segs": 12, "u_pal": themes[startTheme].Pal}
	for i := range coreSlots {
		c.u[coreUniform(i)] = 0
	}
	c.node = caution.Shader(caution.FX{Frag: coresFrag, Uniforms: c.u})
	return c
}

// Set takes per-core percentages (0..100) as the new targets.
func (c *Cores) Set(pct []float64) {
	for i := range coreSlots {
		v := 0.0
		if i < len(pct) && i < c.n {
			v = min(max(pct[i]/100, 0), 1)
		}
		c.target[i] = v
	}
}

func (c *Cores) Ease(alpha float64) bool {
	changed := false
	for i := range coreSlots {
		c.cur[i] = ease(c.cur[i], c.target[i], alpha)
		if c.set(coreUniform(i), round3(c.cur[i])) {
			changed = true
		}
	}
	return changed
}

type Meter struct {
	pane
	target float64
	cur    float64
}

func NewMeter(hue float64) *Meter {
	m := &Meter{}
	m.u = map[string]float64{"u_v": 0, "u_hue": hue, "u_pal": themes[startTheme].Pal}
	m.node = caution.Shader(caution.FX{Frag: meterFrag, Uniforms: m.u})
	return m
}

// Set takes a fraction 0..1 as the new target
func (m *Meter) Set(v float64) { m.target = min(max(v, 0), 1) }

// Ease advances toward the target, reporting whether it moved visibly
func (m *Meter) Ease(alpha float64) bool {
	m.cur = ease(m.cur, m.target, alpha)
	return m.set("u_v", round3(m.cur))
}

// ease is an exponential approach that snaps the last sliver, so a value never
// spends a dozen ticks creeping through a difference nobody can see
func ease(cur, target, alpha float64) float64 {
	d := target - cur
	if d < 0.0005 && d > -0.0005 {
		return target
	}
	return cur + d*alpha
}

// round3 quantises to the precision a meter can actually show
func round3(v float64) float64 { return float64(int(v*1000+0.5)) / 1000 }

type Backdrop struct{ pane }

func NewBackdrop() *Backdrop {
	b := &Backdrop{}
	b.u = map[string]float64{"u_pal": themes[startTheme].Pal}
	b.node = caution.Shader(caution.FX{Frag: backdropFrag, Animate: true, Uniforms: b.u})
	return b
}

// Plate is the header's swept nameplate
type Plate struct{ pane }

func NewPlate() *Plate {
	p := &Plate{}
	p.u = map[string]float64{"u_pal": themes[startTheme].Pal}
	p.node = caution.Shader(caution.FX{Frag: titleFrag, Uniforms: p.u})
	return p
}

type CRT struct {
	node *caution.Node
	pal  float64
}

func Apply(n *caution.Node) *CRT {
	c := &CRT{node: n}
	c.SetPal(themes[startTheme].Pal)
	return c
}

func (c *CRT) SetPal(i float64) {
	c.pal = i
	c.node.Effect(caution.FX{Frag: crtFrag, Uniforms: map[string]float64{"u_pal": i}})
}
