package main

import (
	"fmt"
	"strings"
)

const (
	histWide   = 256 // the full-width cpu chart, ~2 minutes at 500ms
	histNarrow = 128 // the memory and network charts
)

// coreSlots is the per-core meter ceiling
const coreSlots = 32

const palette = `
void pal3(out vec3 lo, out vec3 mid, out vec3 hi) {
  if (u_pal < 0.5) {        // phosphor: the classic green screen
    lo = vec3(0.16, 1.00, 0.55); mid = vec3(0.95, 0.90, 0.30); hi = vec3(1.00, 0.26, 0.36);
  } else if (u_pal < 1.5) { // amber: a warmer old terminal
    lo = vec3(1.00, 0.72, 0.24); mid = vec3(1.00, 0.48, 0.12); hi = vec3(1.00, 0.22, 0.16);
  } else if (u_pal < 2.5) { // ice: cyan through violet
    lo = vec3(0.26, 0.94, 1.00); mid = vec3(0.30, 0.55, 1.00); hi = vec3(0.72, 0.36, 1.00);
  } else if (u_pal < 3.5) { // vaporwave: cyan, purple, hot pink
    lo = vec3(0.00, 0.80, 1.00); mid = vec3(0.73, 0.40, 1.00); hi = vec3(1.00, 0.24, 0.65);
  } else {                  // runner: amber through orange to deep red
    lo = vec3(1.000, 0.698, 0.000); mid = vec3(1.000, 0.416, 0.000); hi = vec3(0.820, 0.039, 0.133);
  }
}

bool isLight() { return u_pal > 3.5; }

vec3 ramp(float t) {
  vec3 lo, mid, hi;
  pal3(lo, mid, hi);
  t = clamp(t, 0.0, 1.0);
  return t < 0.55 ? mix(lo, mid, t / 0.55) : mix(mid, hi, (t - 0.55) / 0.45);
}

vec3 unlitCol() {
  if (isLight()) return vec3(0.045, 0.055, 0.075);
  vec3 lo, mid, hi;
  pal3(lo, mid, hi);
  return lo * 0.085;
}
`

// uniformArray generates the boilerplate that turns N scalar uniforms into an
// indexable array. GLSL has no dynamic indexing of uniforms, but a local
// global array copied from them indexes fine, and the copy costs nothing next
// to the texture work in the same pass.
func uniformArray(name, prefix string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "float %s[%d];\nvoid load%s() {\n", name, n, name)
	for i := range n {
		fmt.Fprintf(&b, "  %s[%d] = %s%03d;\n", name, i, prefix, i)
	}
	b.WriteString("}\n")
	return b.String()
}

// histUniform is the wire name of history slot i. slot 0 is the oldest sample.
func histUniform(i int) string { return fmt.Sprintf("h%03d", i) }

// coreUniform is the wire name of core meter i
func coreUniform(i int) string { return fmt.Sprintf("c%03d", i) }

// graphFragFor builds the chart shader for a given history depth, cached so
// that repeated calls return the identical string
var graphFrags = map[int]string{}

func graphFragFor(n int) string {
	if s, ok := graphFrags[n]; ok {
		return s
	}
	s := buildGraphFrag(n)
	graphFrags[n] = s
	return s
}

// buildGraphFrag is the dot-matrix history chart
//
// u_shift is what makes it glide. samples arrive twice a second, but the
// column grid is offset by a fractional cell that the server advances ~30
// times a second, so the chart scrolls continuously instead of stepping
//
// lookup is a generated binary search rather than a copy of every uniform
// into a local array
func buildGraphFrag(n int) string {
	return palette + histLookup(n) + `
vec4 effect(vec2 uv) {
  vec2 px   = uv * u_res;
  // u_cell is the preferred dot pitch
  float cs  = max(max(u_cell, 2.0), u_res.x / ` + fmt.Sprint(n) + `.0);
  float cols = max(floor(u_res.x / cs), 1.0);
  float rows = max(floor(u_res.y / cs), 1.0);

  // centre the cell grid in the pane so it never clips a half dot at an edge
  vec2 pad  = (u_res - vec2(cols, rows) * cs) * 0.5;
  vec2 g    = (px - pad) / cs;
  g.x += u_shift - 1.0;
  if (g.y < 0.0 || g.y >= rows) return vec4(0.0);

  vec2 cell = floor(g);
  vec2 f    = fract(g) - 0.5;
  float dot = 1.0 - smoothstep(0.17, 0.42, length(f));

  float probe = 0.0;
  if (u_probe > -0.5) {
    probe = 1.0 - step(0.5, abs(cell.x - u_probe));
  }
  if (dot <= 0.0 && probe <= 0.0) return vec4(0.0);

  // newest sample pinned to the right edge
  int back = int(cols - 1.0 - cell.x);
  int idx  = ` + fmt.Sprint(n-1) + ` - back;
  float have = step(0.0, float(idx)) * step(float(idx), ` + fmt.Sprint(n-1) + `.0);
  float v = clamp(hAt(clamp(idx, 0, ` + fmt.Sprint(n-1) + `)), 0.0, 1.0) * have;

  float lvl = (rows - 1.0 - cell.y) / rows;     // 0 at the bottom row
  float amt = clamp((v - lvl) * rows, 0.0, 1.0); // partial dot at the crest

  // arriving column brightens as it slides in
  float fresh = 1.0 - step(0.5, float(` + fmt.Sprint(n-1) + ` - idx));
  float born  = mix(1.0, smoothstep(0.0, 1.0, u_shift), fresh);

  vec3 col = ramp(lvl) * amt * born;
  col += ramp(v) * exp(-abs(lvl - v) * rows * 0.6) * 0.20 * have * born; // crest glow
  col += unlitCol() * (1.0 - amt);                                       // idle field
  col *= dot * u_fade;
  col += ramp(0.0) * probe * (0.05 + 0.35 * dot);                  // scrub cursor

  float a = max(max(col.r, col.g), col.b);
  return vec4(col, a);
}`
}

// coresFrag is a bank of segmented LED meters, one bar per logical core,
// packed into a single pane. u_n is the live core count.
var coresFrag = palette + uniformArray("C", "c", coreSlots) + `
vec4 effect(vec2 uv) {
  loadC();
  float n = max(u_n, 1.0);
  float x = uv.x * n;
  int i = int(floor(x));
  float fx = fract(x);

  // bar body with a gap either side. the gap scales with bar width so a
  // 4-core machine does not get slabs and a 24-core one does not get slivers.
  float gap = clamp(1.6 / (u_res.x / n), 0.04, 0.22);
  float bar = smoothstep(0.0, gap, fx) * smoothstep(1.0, 1.0 - gap, fx);
  if (bar <= 0.0) return vec4(0.0);

  float v    = clamp(C[clamp(i, 0, ` + fmt.Sprint(coreSlots-1) + `)], 0.0, 1.0);
  float segs = max(u_segs, 3.0);
  float lvl  = 1.0 - uv.y;
  float seg  = floor(lvl * segs);
  float sv   = seg / segs;

  // Segment separator lines.
  float fy   = fract(lvl * segs);
  float mask = smoothstep(0.0, 0.16, fy) * smoothstep(1.0, 0.84, fy);
  float on   = step(sv, v - 0.0001);

  vec3 col = (ramp(sv) * on + unlitCol() * (1.0 - on)) * mask * bar;
  float a = max(max(col.r, col.g), col.b);
  return vec4(col, a);
}`

const meterFrag = palette + `
vec3 tint(vec3 c, float h) {
  vec3 alt = vec3(c.b * 0.5 + c.g * 0.35, c.g * 0.55 + c.r * 0.15, c.r * 0.75 + c.g * 0.5);
  return mix(c, alt, clamp(h, 0.0, 1.0));
}

vec4 effect(vec2 uv) {
  float segs = max(floor(u_res.x / 6.0), 8.0);
  float s    = floor(uv.x * segs);
  float sv   = s / segs;
  float fx   = fract(uv.x * segs);
  float mask = smoothstep(0.0, 0.30, fx) * smoothstep(1.0, 0.70, fx);
  float vmask = smoothstep(0.08, 0.92, 1.0 - abs(uv.y - 0.5) * 2.0);

  float on = step(sv, clamp(u_v, 0.0, 1.0) - 0.0001);
  vec3 col = (tint(ramp(sv), u_hue) * on + unlitCol() * (1.0 - on)) * mask * vmask;

  float head = exp(-abs(sv - u_v) * segs * 0.9) * on;
  col += tint(ramp(u_v), u_hue) * head * 0.5 * mask * vmask;

  float a = max(max(col.r, col.g), col.b);
  return vec4(col, a);
}`

var backdropFrag = palette + `
float hash(vec2 p) { return fract(sin(dot(p, vec2(127.1, 311.7))) * 43758.5453); }

vec3 vapor(vec2 uv) {
  const float horizon = 0.46;
  float aspect = u_res.x / u_res.y;

  if (uv.y < horizon) {
    float t = uv.y / horizon;                          // 0 sky top, 1 horizon
    vec3 col = mix(vec3(0.055, 0.020, 0.150), vec3(0.330, 0.055, 0.290), t * t);

    // stars, thinning out toward the horizon haze
    vec2 cell = floor(uv * u_res / 3.0);
    float star = step(0.9992, hash(cell)) * (1.0 - t) * 0.8;
    col += vec3(0.9, 0.85, 1.0) * star;

    // sun
    vec2 sc = vec2((uv.x - 0.5) * aspect, uv.y - (horizon - 0.17));
    float d = length(sc);
    float disc = smoothstep(0.172, 0.166, d);
    float band = fract((uv.y - (horizon - 0.33)) * u_res.y / 11.0);
    float slit = step(0.40, band);
    float slitIn = smoothstep(-0.04, 0.13, sc.y);      // solid at the top
    disc *= mix(1.0, slit, slitIn);
    vec3 sunCol = mix(vec3(1.00, 0.86, 0.35), vec3(1.00, 0.18, 0.55),
                      clamp((sc.y + 0.17) / 0.34, 0.0, 1.0));
    col = mix(col, sunCol, disc);
    col += vec3(1.0, 0.25, 0.60) * exp(-d * 7.0) * 0.22;   // bloom
    return col;
  }

  // floor: 1/t is the perspective depth, so lines bunch up at the horizon.
  float t = uv.y - horizon;
  float z = 1.0 / (t + 0.015);
  float depth = exp(-t * 3.4);                         // fade with distance

  vec3 col = mix(vec3(0.085, 0.015, 0.150), vec3(0.020, 0.005, 0.045), t * 2.2);

  float gz = fract(z * 0.22 - u_time * 0.5);
  float lineZ = smoothstep(0.055, 0.0, min(gz, 1.0 - gz));
  float xw = (uv.x - 0.5) * aspect * z * 0.30;
  float gx = fract(xw);
  float lineX = smoothstep(0.045, 0.0, min(gx, 1.0 - gx));

  col += vec3(0.10, 0.90, 1.00) * lineZ * depth * 0.80;   // cyan, receding
  col += vec3(1.00, 0.25, 0.75) * lineX * depth * 0.62;   // pink, converging
  col += vec3(0.55, 0.20, 0.85) * exp(-t * 26.0) * 0.30;  // horizon glow
  return col;
}

vec3 daylight(vec2 uv) {
  vec2 px = uv * u_res;
  float aspect = u_res.x / u_res.y;

  vec3 col = mix(vec3(0.855, 0.905, 0.950), vec3(0.960, 0.965, 0.970),
                 smoothstep(0.0, 0.62, uv.y));

  vec2 sc = vec2((uv.x - 0.80) * aspect, uv.y - 0.06);
  col += vec3(1.0, 0.98, 0.93) * exp(-dot(sc, sc) * 14.0) * 0.32;

  col -= vec3(0.05, 0.045, 0.035) * smoothstep(0.9925, 1.0, fract(px.x / 260.0));
  col -= vec3(0.04, 0.035, 0.030) * smoothstep(0.9955, 1.0, fract(uv.y * 3.0 + 0.5));

  float pulse = 0.55 + 0.45 * sin(u_time * 0.5);
  float guide = smoothstep(0.9958, 1.0, fract(px.x / 700.0 + 0.18))
              + smoothstep(0.9972, 1.0, fract(px.y / 520.0 + 0.40));
  col = mix(col, vec3(0.910, 0.067, 0.176), clamp(guide, 0.0, 1.0) * 0.5 * pulse);
  return col;
}

vec4 effect(vec2 uv) {
  if (u_pal > 3.5) return vec4(daylight(uv), 1.0);
  if (u_pal > 2.5) return vec4(vapor(uv), 1.0);

  vec2 px = uv * u_res;
  vec3 tint = ramp(0.0);                       // the theme's own phosphor
  vec3 col = mix(tint * 0.030, tint * 0.010, uv.y);

  // static square grid, brighter on the majors
  vec2 g  = abs(fract(px / 26.0) - 0.5);
  float minor = smoothstep(0.47, 0.5, max(g.x, g.y));
  vec2 G  = abs(fract(px / 130.0) - 0.5);
  float major = smoothstep(0.487, 0.5, max(G.x, G.y));
  col += tint * 0.055 * minor;
  col += tint * 0.095 * major;

  // data rain
  float colw = 13.0;
  float ci   = floor(px.x / colw);
  float seed = hash(vec2(ci, 3.0));
  if (seed > 0.80) {
    float speed = 26.0 + seed * 80.0;
    float y     = fract((px.y - u_time * speed) / (u_res.y * 0.7));
    float trail = exp(-y * 9.0);
    float glyph = step(0.5, hash(vec2(ci, floor(px.y / 8.0) - floor(u_time * speed / 8.0))));
    float band  = smoothstep(0.0, 0.3, fract(px.x / colw)) * smoothstep(1.0, 0.7, fract(px.x / colw));
    col += tint * trail * glyph * band * 0.16;
  }

  // horizon sweep and a faint film of noise so flat areas never band
  col += tint * 0.10 * exp(-14.0 * abs(fract(uv.y - u_time * 0.06) - 0.5));
  col += (hash(px + u_time) - 0.5) * 0.012;
  return vec4(col, 1.0);
}`

var crtFrag = palette + `
vec4 effect(vec2 uv) {
  vec2 c = uv * 2.0 - 1.0;
  c *= 1.0 + 0.006 * dot(c, c);
  // clamped so the barrel warp cannot sample past the layer, where src() is
  // transparent
  vec2 s = clamp((c + 1.0) * 0.5, 0.0, 1.0);

  // channel separation is expressed in pixels and converted to uv, because a
  // constant in uv space scales with the window
  float ab = (0.06 + 0.34 * dot(c, c)) / u_res.x;
  vec4 col;
  col.r = src(clamp(s + vec2(ab, 0.0), 0.0, 1.0)).r;
  col.g = src(s).g;
  col.b = src(clamp(s - vec2(ab, 0.0), 0.0, 1.0)).b;
  col.a = src(s).a;

  col.rgb *= 0.975 + 0.025 * sin(s.y * u_res.y * 3.14159);    // scanlines
  col.rgb += col.rgb * exp(-26.0 * abs(fract(s.y - u_time * 0.07) - 0.5)) * 0.07;
  col.rgb *= 1.0 - 0.10 * dot(c, c);                          // vignette

  return col;
}`

// titleFrag paints the header's nameplate
var titleFrag = palette + `
vec4 effect(vec2 uv) {
  if (isLight()) {
    // Opaque, so the titlebar reads as chrome rather than as a translucent
    // panel floating over the city.
    vec3 col = mix(vec3(1.0), vec3(0.945, 0.955, 0.965), uv.y);
    col = mix(col, vec3(0.910, 0.067, 0.176), smoothstep(0.94, 1.0, uv.y));
    return vec4(col, 1.0);
  }
  vec3 tint = ramp(0.0);
  float sweep = exp(-8.0 * abs(fract(uv.x * 0.5 - u_time * 0.08) - 0.5));
  vec3 col = mix(tint * 0.10, tint * 0.035, uv.y);
  col += tint * sweep * 0.16;
  col += tint * 0.13 * smoothstep(0.86, 1.0, 1.0 - uv.y); // lit top edge
  float a = 0.86;
  return vec4(col * a, a);
}`

// histLookup generates a binary search over the history uniforms, so a
// fragment reads the one sample its column needs instead of
// copying all 128 into a local array first
func histLookup(n int) string {
	var b strings.Builder
	b.WriteString("float hAt(int i) {\n")
	var emit func(lo, hi, depth int)
	emit = func(lo, hi, depth int) {
		ind := strings.Repeat("  ", depth+1)
		if hi-lo == 1 {
			fmt.Fprintf(&b, "%sreturn %s;\n", ind, histUniform(lo))
			return
		}
		mid := (lo + hi) / 2
		fmt.Fprintf(&b, "%sif (i < %d) {\n", ind, mid)
		emit(lo, mid, depth+1)
		fmt.Fprintf(&b, "%s} else {\n", ind)
		emit(mid, hi, depth+1)
		fmt.Fprintf(&b, "%s}\n", ind)
	}
	emit(0, n, 0)
	b.WriteString("}\n")
	return b.String()
}
