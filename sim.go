package main

import (
	"math"
	"math/rand/v2"
	"time"
)

// Default LD2410C factory thresholds, used by -sim.
var simParams = &params{
	motionThr: []byte{50, 50, 40, 30, 20, 15, 15, 15, 15},
	staticThr: []byte{0, 0, 40, 40, 30, 30, 20, 20, 20},
	noOneSec:  5,
}

// simulator fakes engineering-mode reports: a person walks toward and away
// from the sensor, stops to stand still, and a fan adds static noise at a
// far gate. Handy for working on the UI without hardware.
type simulator struct {
	t0, due time.Time
}

func newSimulator() *simulator { return &simulator{t0: time.Now(), due: time.Now()} }

func (s *simulator) next() report {
	// 10 Hz like the real sensor, independent of render time.
	s.due = s.due.Add(100 * time.Millisecond)
	time.Sleep(time.Until(s.due))
	t := time.Since(s.t0).Seconds()

	// 24 s cycle: walk in (0-6s), stand (6-12s), walk out (12-18s), stand (18-24s).
	const near, far = 0.8, 5.2
	var dist float64
	moving := true
	switch c := math.Mod(t, 24); {
	case c < 6:
		dist = far - (far-near)*smooth(c/6)
	case c < 12:
		dist, moving = near, false
	case c < 18:
		dist = near + (far-near)*smooth((c-12)/6)
	default:
		dist, moving = far, false
	}
	breath := 0.5 + 0.5*math.Sin(2*math.Pi*t/4) // ~15 breaths/min

	const gates = 9
	motion := make([]byte, gates)
	static := make([]byte, gates)
	g := dist / gateWidthM
	for i := range gates {
		d := float64(i) + 0.5 - g
		blob := math.Exp(-d * d / 0.8)
		// Energy falls off with range.
		fall := 1 / (1 + 0.25*g)
		m := 4 + 6*rand.Float64()
		st := 3 + 4*rand.Float64()
		if moving {
			m += 95 * blob * fall * (0.8 + 0.2*rand.Float64())
			st += 30 * blob * fall
		} else {
			m += (8 + 10*breath) * blob * fall
			st += (70 + 15*breath) * blob * fall
		}
		if i == 7 { // ceiling fan
			st += 18 + 6*math.Sin(2*math.Pi*t*1.3)
			m += 8 * rand.Float64()
		}
		motion[i] = clamp100(m)
		static[i] = clamp100(st)
	}

	rep := report{eng: true, motion: motion, static: static}
	cm := uint16(dist * 100)
	if moving {
		rep.state = 3
		rep.movDist, rep.movE = cm, motion[min(int(g), gates-1)]
	} else {
		rep.state = 2
	}
	rep.statDist, rep.statE = cm, static[min(int(g), gates-1)]
	rep.detDst = cm
	return rep
}

func smooth(x float64) float64 { return x * x * (3 - 2*x) }

func clamp100(v float64) byte { return byte(math.Max(0, math.Min(100, v))) }
