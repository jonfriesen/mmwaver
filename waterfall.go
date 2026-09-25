package main

import (
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// waterfall renders a scrolling range-time heatmap of per-gate energies.
//
// Two panels (motion on top, static below) share the same layout: range on
// the Y axis (near at the bottom), time on the X axis (newest at the right),
// one column per report. Each terminal cell holds two vertical pixels via
// the upper-half-block glyph, and energy is interpolated between gate
// centers so the picture reads as a continuous range profile.
type waterfall struct {
	prm   *params
	hist  []report
	total int // reports seen; keeps the dotted trace phase stable while scrolling

	cols, rows int

	lastAt time.Time
	hz     float64
}

const (
	leftGutter  = 9 // "g8 6.4m ┤"
	rightGutter = 1
	// Visual floor applied to energy below the gate threshold, so a
	// detection pops out against background.
	belowThrDim = 0.45
)

func newWaterfall(prm *params) *waterfall {
	w := &waterfall{prm: prm}
	w.resize()
	return w
}

func (w *waterfall) start() {
	// Alternate screen, hide cursor, clear.
	os.Stdout.WriteString("\033[?1049h\033[?25l\033[2J")
}

func (w *waterfall) stop() {
	os.Stdout.WriteString("\033[0m\033[?25h\033[?1049l")
}

func (w *waterfall) resize() {
	w.cols, w.rows = 100, 32
	if ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ); err == nil && ws.Col > 0 {
		w.cols, w.rows = int(ws.Col), int(ws.Row)
	}
	os.Stdout.WriteString("\033[2J")
}

func (w *waterfall) graphWidth() int { return max(10, w.cols-leftGutter-rightGutter) }

func (w *waterfall) push(rep report) {
	now := time.Now()
	if !w.lastAt.IsZero() {
		if dt := now.Sub(w.lastAt).Seconds(); dt > 0 {
			inst := 1 / dt
			if w.hz == 0 {
				w.hz = inst
			} else {
				w.hz += 0.1 * (inst - w.hz)
			}
		}
	}
	w.lastAt = now

	w.total++
	w.hist = append(w.hist, rep)
	// Keep a little more than one screen so resizing wider shows history.
	if keep := 2 * w.graphWidth(); len(w.hist) > keep {
		w.hist = append(w.hist[:0], w.hist[len(w.hist)-keep:]...)
	}
}

// ---- colour ---------------------------------------------------------------

type rgb struct{ r, g, b uint8 }

// Inferno-like ramp: black -> purple -> red -> orange -> pale yellow.
var ramp = []struct {
	t float64
	c rgb
}{
	{0.00, rgb{0, 0, 4}},
	{0.15, rgb{31, 12, 72}},
	{0.35, rgb{101, 21, 110}},
	{0.55, rgb{188, 55, 84}},
	{0.75, rgb{249, 142, 9}},
	{1.00, rgb{252, 255, 164}},
}

func heat(t float64) rgb {
	t = math.Max(0, math.Min(1, t))
	t = math.Pow(t, 0.75) // lift low energies a little
	for i := 1; i < len(ramp); i++ {
		if t <= ramp[i].t {
			a, b := ramp[i-1], ramp[i]
			f := (t - a.t) / (b.t - a.t)
			lerp := func(x, y uint8) uint8 { return uint8(float64(x) + f*(float64(y)-float64(x))) }
			return rgb{lerp(a.c.r, b.c.r), lerp(a.c.g, b.c.g), lerp(a.c.b, b.c.b)}
		}
	}
	return ramp[len(ramp)-1].c
}

var (
	traceMotion = rgb{120, 255, 230} // cyan: reported moving-target distance
	traceStatic = rgb{120, 200, 255} // blue: reported still-target distance
)

// ---- sampling -------------------------------------------------------------

// sample returns the colour for range r (in gate units, 0 = sensor) given
// per-gate energies and thresholds.
func sample(e, thr []byte, r float64) rgb {
	if len(e) == 0 {
		return rgb{}
	}
	// Gate g covers [g, g+1); its centre is g+0.5.
	x := r - 0.5
	g0 := int(math.Floor(x))
	f := x - float64(g0)
	g1 := g0 + 1
	g0 = max(0, min(g0, len(e)-1))
	g1 = max(0, min(g1, len(e)-1))
	v := (1-f)*float64(e[g0]) + f*float64(e[g1])

	t := v / 100
	near := int(math.Floor(r))
	if near >= 0 && near < len(thr) && near < len(e) && int(e[near]) <= int(thr[near]) {
		t *= belowThrDim
	}
	return heat(t)
}

// ---- rendering ------------------------------------------------------------

type cellWriter struct {
	sb     strings.Builder
	fg, bg rgb
	hasFg  bool
	hasBg  bool
}

func (cw *cellWriter) reset() {
	cw.sb.WriteString("\033[0m")
	cw.hasFg, cw.hasBg = false, false
}

func (cw *cellWriter) setFg(c rgb) {
	if cw.hasFg && cw.fg == c {
		return
	}
	fmt.Fprintf(&cw.sb, "\033[38;2;%d;%d;%dm", c.r, c.g, c.b)
	cw.fg, cw.hasFg = c, true
}

func (cw *cellWriter) setBg(c rgb) {
	if cw.hasBg && cw.bg == c {
		return
	}
	fmt.Fprintf(&cw.sb, "\033[48;2;%d;%d;%dm", c.r, c.g, c.b)
	cw.bg, cw.hasBg = c, true
}

func (cw *cellWriter) text(fg rgb, s string) {
	if cw.hasBg {
		cw.reset()
	}
	cw.setFg(fg)
	cw.sb.WriteString(s)
}

var (
	cText  = rgb{220, 220, 220}
	cDim   = rgb{120, 120, 130}
	cTitle = rgb{255, 190, 90}
)

var stateColor = []rgb{
	{140, 140, 140}, // none
	{120, 255, 230}, // moving
	{120, 200, 255}, // still
	{255, 120, 200}, // both
}

func (w *waterfall) render() {
	gw := w.graphWidth()

	var gates int
	if n := len(w.hist); n > 0 {
		gates = max(len(w.hist[n-1].motion), len(w.hist[n-1].static))
	}
	if gates == 0 {
		gates = 9
	}

	// Rows: header(1) + blank(1) + 2 * (label(1) + panel) + axis(1) + legend(1)
	panelRows := max(gates/2+1, (w.rows-6)/2)

	cw := &cellWriter{}
	cw.sb.Grow(w.cols * w.rows * 24)
	cw.sb.WriteString("\033[H")

	w.header(cw)
	cw.reset()
	cw.sb.WriteString("\033[K\n")

	var mThr, sThr []byte
	if w.prm != nil {
		mThr, sThr = w.prm.motionThr, w.prm.staticThr
	}
	w.panel(cw, "MOTION energy", gw, panelRows, gates, true, mThr)
	w.panel(cw, "STATIC energy", gw, panelRows, gates, false, sThr)
	w.footer(cw, gw)

	cw.reset()
	cw.sb.WriteString("\033[J")
	os.Stdout.WriteString(cw.sb.String())
}

func (w *waterfall) header(cw *cellWriter) {
	cw.text(cTitle, " LD2410C range-time waterfall ")
	if len(w.hist) == 0 {
		cw.text(cDim, " waiting for engineering-mode frames...")
		return
	}
	rep := w.hist[len(w.hist)-1]
	st := int(rep.state)
	if st >= len(states) {
		st = 0
	}
	cw.text(cDim, " state ")
	cw.text(stateColor[st], fmt.Sprintf("%-6s", states[st]))
	cw.text(cDim, "  moving ")
	cw.text(cText, fmt.Sprintf("%4.2fm e%-3d", float64(rep.movDist)/100, rep.movE))
	cw.text(cDim, "  still ")
	cw.text(cText, fmt.Sprintf("%4.2fm e%-3d", float64(rep.statDist)/100, rep.statE))
	cw.text(cDim, "  det ")
	cw.text(cText, fmt.Sprintf("%4.2fm", float64(rep.detDst)/100))
	cw.text(cDim, fmt.Sprintf("  %4.1f Hz", w.hz))
}

// panel draws one heatmap. Pixel row p (0 = top) of P maps to range
// r = (P - p - 0.5) / P * gates, in gate units.
func (w *waterfall) panel(cw *cellWriter, title string, gw, rows, gates int, motion bool, thr []byte) {
	cw.reset()
	cw.text(cText, fmt.Sprintf(" %-*s", leftGutter-1, ""))
	cw.text(cText, title)
	if thr == nil {
		cw.text(cDim, "  (thresholds unknown)")
	}
	cw.reset()
	cw.sb.WriteString("\033[K\n")

	P := rows * 2
	pixRange := func(p int) float64 { return (float64(P-p) - 0.5) / float64(P) * float64(gates) }

	// Which terminal row gets each gate's label: the row containing the
	// gate centre.
	labelRow := make(map[int]int, gates)
	for g := range gates {
		centre := float64(g) + 0.5
		p := int(float64(P) - centre/float64(gates)*float64(P))
		labelRow[p/2] = g
	}

	n := len(w.hist)
	start := n - gw // may be negative: pad on the left

	// Pixel row of the reported target distance for each column (-1: none).
	trace := make([]int, gw)
	for x := range gw {
		trace[x] = -1
		i := start + x
		// Dotted so the heatmap underneath stays visible.
		if i < 0 || (w.total-n+i)%2 != 0 {
			continue
		}
		rep := w.hist[i]
		var cm uint16
		var on bool
		if motion {
			cm, on = rep.movDist, rep.state == 1 || rep.state == 3
		} else {
			cm, on = rep.statDist, rep.state == 2 || rep.state == 3
		}
		if !on {
			continue
		}
		r := float64(cm) / 100 / gateWidthM // gate units
		p := int(math.Round(float64(P) - r/float64(gates)*float64(P) - 0.5))
		if p >= 0 && p < P {
			trace[x] = p
		}
	}
	tc := traceStatic
	if motion {
		tc = traceMotion
	}

	for row := range rows {
		cw.reset()
		if g, ok := labelRow[row]; ok {
			cw.text(cDim, fmt.Sprintf("g%d %4.1fm┤", g, (float64(g)+0.5)*gateWidthM))
		} else {
			cw.text(cDim, fmt.Sprintf("%*s│", leftGutter-1, ""))
		}

		pt, pb := row*2, row*2+1
		rt, rb := pixRange(pt), pixRange(pb)
		for x := range gw {
			i := start + x
			var top, bot rgb
			if i >= 0 {
				rep := w.hist[i]
				e := rep.static
				if motion {
					e = rep.motion
				}
				top, bot = sample(e, thr, rt), sample(e, thr, rb)
				if trace[x] == pt {
					top = tc
				}
				if trace[x] == pb {
					bot = tc
				}
			}
			cw.setFg(top)
			cw.setBg(bot)
			cw.sb.WriteString("▀")
		}
		cw.reset()
		cw.sb.WriteString("\033[K\n")
	}
}

func (w *waterfall) footer(cw *cellWriter, gw int) {
	// Time axis.
	cw.reset()
	cw.text(cDim, fmt.Sprintf("%*s└", leftGutter-1, ""))
	span := ""
	if w.hz > 0 {
		span = fmt.Sprintf("◀ %.0fs ", float64(gw)/w.hz)
	}
	now := " now ▶"
	fill := max(0, gw-len([]rune(span))-len([]rune(now)))
	cw.text(cDim, span+strings.Repeat("─", fill)+now)
	cw.sb.WriteString("\033[K\n")

	// Legend.
	cw.text(cDim, fmt.Sprintf("%*s", leftGutter, "0 "))
	const barLen = 24
	for i := range barLen {
		cw.setBg(heat(float64(i) / float64(barLen-1)))
		cw.sb.WriteString(" ")
	}
	cw.reset()
	cw.text(cDim, " 100  dim = below threshold  ")
	cw.setBg(traceMotion)
	cw.sb.WriteString(" ")
	cw.reset()
	cw.text(cDim, " moving target  ")
	cw.setBg(traceStatic)
	cw.sb.WriteString(" ")
	cw.reset()
	cw.text(cDim, " still target   Ctrl-C to exit")
	cw.sb.WriteString("\033[K")
}
