// Command mmwaver reads target reports from an HLK LD2410C mmWave radar
// over a USB-UART adapter (e.g. CP2102).
//
// Usage:
//
//	go run . /dev/cu.usbserial-0001        # basic one-line reports, 1/s
//	go run . -format json -interval 0 ...  # every report as JSON lines
//	go run . -eng /dev/cu.usbserial-0001   # engineering mode: per-gate energies
//	go run . -wf /dev/cu.usbserial-0001    # range-time waterfall (implies -eng)
//	go run . -wf -sim                      # waterfall with synthetic data
//	go run .                               # no port given: pick from a list
//	go run . set-gate -static 60 7 ...     # raise gate 7's static threshold
//	go run . reset ...                     # factory defaults
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.bug.st/serial"
)

var (
	dataHead = []byte{0xF4, 0xF3, 0xF2, 0xF1}
	dataTail = []byte{0xF8, 0xF7, 0xF6, 0xF5}
	ackHead  = []byte{0xFD, 0xFC, 0xFB, 0xFA}
	ackTail  = []byte{0x04, 0x03, 0x02, 0x01}

	errTimeout  = errors.New("read timeout")
	errBadFrame = errors.New("bad frame")
)

// Command words.
const (
	cmdEnableConfig = 0x00FF
	cmdEndConfig    = 0x00FE
	cmdReadParams   = 0x0061
	cmdEngOn        = 0x0062
	cmdEngOff       = 0x0063
)

const gateWidthM = 0.75 // default distance resolution

var states = []string{"none", "moving", "still", "both"}

// portReader turns go.bug.st/serial's (0, nil) timeout into an error so
// bufio doesn't spin.
type portReader struct{ p serial.Port }

func (pr portReader) Read(b []byte) (int, error) {
	n, err := pr.p.Read(b)
	if n == 0 && err == nil {
		return 0, errTimeout
	}
	return n, err
}

func soft(err error) bool {
	return errors.Is(err, errTimeout) || errors.Is(err, errBadFrame)
}

type frame struct {
	ack bool
	p   []byte // payload between length and tail
}

func advance(m int, b byte, pat []byte) int {
	if b == pat[m] {
		return m + 1
	}
	if b == pat[0] {
		return 1
	}
	return 0
}

// readFrame syncs on either a data report or a command ACK header.
func readFrame(r *bufio.Reader) (frame, error) {
	var dm, am int
	for dm < 4 && am < 4 {
		b, err := r.ReadByte()
		if err != nil {
			return frame{}, err
		}
		dm = advance(dm, b, dataHead)
		am = advance(am, b, ackHead)
	}
	ack := am == 4

	var lb [2]byte
	if _, err := io.ReadFull(r, lb[:]); err != nil {
		return frame{}, err
	}
	n := int(binary.LittleEndian.Uint16(lb[:]))
	if n == 0 || n > 128 {
		return frame{}, errBadFrame
	}
	buf := make([]byte, n+4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return frame{}, err
	}
	tail := dataTail
	if ack {
		tail = ackTail
	}
	if !bytes.Equal(buf[n:], tail) {
		return frame{}, errBadFrame
	}
	return frame{ack: ack, p: buf[:n]}, nil
}

// sendCmd writes a command frame and waits for its ACK, skipping any data
// reports still in flight. Returns the ACK payload after the status word.
func sendCmd(port serial.Port, r *bufio.Reader, cmd uint16, value []byte) ([]byte, error) {
	out := append([]byte{}, ackHead...)
	out = binary.LittleEndian.AppendUint16(out, uint16(2+len(value)))
	out = binary.LittleEndian.AppendUint16(out, cmd)
	out = append(out, value...)
	out = append(out, ackTail...)

	for range 3 {
		if _, err := port.Write(out); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			f, err := readFrame(r)
			if err != nil {
				if soft(err) {
					continue
				}
				return nil, err
			}
			if !f.ack || len(f.p) < 4 || binary.LittleEndian.Uint16(f.p) != cmd|0x0100 {
				continue
			}
			if status := binary.LittleEndian.Uint16(f.p[2:]); status != 0 {
				return nil, fmt.Errorf("command 0x%04X failed: status %d", cmd, status)
			}
			return f.p[4:], nil
		}
	}
	return nil, fmt.Errorf("command 0x%04X: no ACK", cmd)
}

// withConfig wraps commands in enable/end config mode.
func withConfig(port serial.Port, r *bufio.Reader, fn func() error) error {
	if _, err := sendCmd(port, r, cmdEnableConfig, []byte{0x01, 0x00}); err != nil {
		return err
	}
	ferr := fn()
	if _, err := sendCmd(port, r, cmdEndConfig, nil); err != nil && ferr == nil {
		ferr = err
	}
	return ferr
}

type params struct {
	motionThr, staticThr []byte
	noOneSec             uint16
}

func parseParams(p []byte) (*params, error) {
	// AA, maxGate, maxMovGate, maxStatGate, motion[9], static[9], duration(2)
	if len(p) < 24 || p[0] != 0xAA {
		return nil, errBadFrame
	}
	n := int(p[1]) + 1
	if len(p) < 4+2*n+2 {
		return nil, errBadFrame
	}
	return &params{
		motionThr: p[4 : 4+n],
		staticThr: p[4+n : 4+2*n],
		noOneSec:  binary.LittleEndian.Uint16(p[4+2*n:]),
	}, nil
}

type report struct {
	state                     byte
	movDist, statDist, detDst uint16
	movE, statE               byte

	eng            bool
	motion, static []byte
	extra          []byte // light sensor, OUT pin (firmware dependent)
}

func parseReport(p []byte) (report, bool) {
	if len(p) < 13 || p[1] != 0xAA || p[len(p)-2] != 0x55 {
		return report{}, false
	}
	d := p[2 : len(p)-2]
	if len(d) < 9 || d[0] > 3 {
		return report{}, false
	}
	rep := report{
		state:    d[0],
		movDist:  binary.LittleEndian.Uint16(d[1:3]),
		movE:     d[3],
		statDist: binary.LittleEndian.Uint16(d[4:6]),
		statE:    d[6],
		detDst:   binary.LittleEndian.Uint16(d[7:9]),
	}
	if p[0] == 0x01 && len(d) >= 11 {
		nm, ns := int(d[9]), int(d[10])
		if len(d) >= 13+nm+ns {
			rep.eng = true
			rep.motion = d[11 : 12+nm]
			rep.static = d[12+nm : 13+nm+ns]
			rep.extra = d[13+nm+ns:]
		}
	}
	return rep, true
}

const csvHeader = "time,state,moving_cm,moving_energy,still_cm,still_energy,detect_cm"

func printBasic(rep report, format string) {
	now := time.Now()
	switch format {
	case "json":
		json.NewEncoder(os.Stdout).Encode(struct {
			Time     time.Time `json:"time"`
			State    string    `json:"state"`
			MovingCm uint16    `json:"moving_cm"`
			MovingE  byte      `json:"moving_energy"`
			StillCm  uint16    `json:"still_cm"`
			StillE   byte      `json:"still_energy"`
			DetectCm uint16    `json:"detect_cm"`
		}{now, states[rep.state], rep.movDist, rep.movE, rep.statDist, rep.statE, rep.detDst})
	case "csv":
		fmt.Printf("%s,%s,%d,%d,%d,%d,%d\n", now.Format(time.RFC3339Nano),
			states[rep.state], rep.movDist, rep.movE, rep.statDist, rep.statE, rep.detDst)
	default:
		fmt.Printf("state=%-6s moving=%3dcm e=%3d  still=%3dcm e=%3d  det=%3dcm\n",
			states[rep.state], rep.movDist, rep.movE, rep.statDist, rep.statE, rep.detDst)
	}
}

const barWidth = 20

// bar draws energy v (0-100) with a threshold marker at thr (<0 for none).
func bar(v, thr int) string {
	b := make([]rune, barWidth)
	fill := v * barWidth / 100
	for i := range b {
		if i < fill {
			b[i] = '█'
		} else {
			b[i] = '·'
		}
	}
	if thr >= 0 {
		t := min(thr*barWidth/100, barWidth-1)
		if t < fill {
			b[t] = '┃'
		} else {
			b[t] = '|'
		}
	}
	return string(b)
}

func at(s []byte, i int) int {
	if i < len(s) {
		return int(s[i])
	}
	return -1
}

func renderEng(rep report, prm *params) {
	var sb strings.Builder
	sb.WriteString("\033[H\033[J") // cursor home, clear screen
	fmt.Fprintf(&sb, "state=%-6s moving=%3dcm e=%3d  still=%3dcm e=%3d  det=%3dcm\n",
		states[rep.state], rep.movDist, rep.movE, rep.statDist, rep.statE, rep.detDst)
	if len(rep.extra) >= 2 {
		fmt.Fprintf(&sb, "light=%d out=%d", rep.extra[0], rep.extra[1])
	}
	if prm != nil {
		fmt.Fprintf(&sb, "  no-one timeout=%ds", prm.noOneSec)
	}
	sb.WriteString("\n\n")
	fmt.Fprintf(&sb, "gate  range         %-26s %s\n", "motion", "static")

	gates := max(len(rep.motion), len(rep.static))
	for g := range gates {
		mv, sv := at(rep.motion, g), at(rep.static, g)
		mt, st := -1, -1
		if prm != nil {
			mt, st = at(prm.motionThr, g), at(prm.staticThr, g)
		}
		fmt.Fprintf(&sb, "%2d    %.2f-%.2fm  %s %s   %s %s\n",
			g, float64(g)*gateWidthM, float64(g+1)*gateWidthM,
			bar(max(mv, 0), mt), cell(mv, mt),
			bar(max(sv, 0), st), cell(sv, st))
	}
	sb.WriteString("\n█ energy   | threshold   * over threshold   Ctrl-C to exit\n")
	os.Stdout.WriteString(sb.String())
}

func cell(v, thr int) string {
	if v < 0 {
		return "   "
	}
	mark := " "
	if thr >= 0 && v > thr {
		mark = "*"
	}
	return fmt.Sprintf("%3d%s", v, mark)
}

// resolvePort returns arg, or the user's pick when arg is empty. Exits on cancel.
func resolvePort(arg string) string {
	if arg != "" {
		return arg
	}
	name, err := pickPort()
	if err != nil {
		log.Fatal(err)
	}
	if name == "" {
		os.Exit(0)
	}
	return name
}

func main() {
	eng := flag.Bool("eng", false, "enable engineering mode (per-gate energies)")
	wf := flag.Bool("wf", false, "range-time waterfall display (implies -eng)")
	sim := flag.Bool("sim", false, "use synthetic data instead of a sensor")
	format := flag.String("format", "text", "basic-mode output format: text, json, or csv")
	interval := flag.Duration("interval", time.Second, "basic-mode minimum time between reports (0 = every report)")
	baud := flag.Int("baud", 256000, "serial baud rate")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: mmwaver [flags] [serial-port]
       mmwaver [flags] -sim
       mmwaver [-baud N] set-gate [-motion N] [-static N] <gate|all> [serial-port]
       mmwaver [-baud N] reset [serial-port]
Omit serial-port to pick from a list.`)
		flag.PrintDefaults()
	}
	flag.Parse()
	if *wf || *sim {
		*eng = true
	}
	switch *format {
	case "text", "json", "csv":
	default:
		log.Fatalf("unknown -format %q", *format)
	}
	switch flag.Arg(0) {
	case "set-gate":
		if err := setGate(flag.Args()[1:], *baud); err != nil {
			log.Fatal(err)
		}
		return
	case "reset":
		if err := factoryReset(flag.Args()[1:], *baud); err != nil {
			log.Fatal(err)
		}
		return
	}
	portName := ""
	if !*sim {
		portName = resolvePort(flag.Arg(0))
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)

	// next yields the next report; cleanup undoes any sensor config.
	var (
		next    func() (report, error)
		cleanup = func() {}
		prm     *params
	)

	if *sim {
		s := newSimulator()
		prm = simParams
		next = func() (report, error) { return s.next(), nil }
	} else {
		port, r, err := openPort(portName, *baud)
		if err != nil {
			log.Fatal(err)
		}
		defer port.Close()

		if *eng {
			err := withConfig(port, r, func() error {
				p, err := sendCmd(port, r, cmdReadParams, nil)
				if err != nil {
					return err
				}
				if prm, err = parseParams(p); err != nil {
					log.Printf("could not parse thresholds: %v", err)
				}
				_, err = sendCmd(port, r, cmdEngOn, nil)
				return err
			})
			if err != nil {
				log.Fatalf("enable engineering mode: %v", err)
			}
			cleanup = func() {
				if err := withConfig(port, r, func() error {
					_, err := sendCmd(port, r, cmdEngOff, nil)
					return err
				}); err != nil {
					log.Printf("disable engineering mode: %v", err)
				}
			}
		}

		next = func() (report, error) {
			for {
				f, err := readFrame(r)
				if err != nil {
					if soft(err) {
						return report{}, errTimeout
					}
					return report{}, err
				}
				if f.ack {
					continue
				}
				if rep, ok := parseReport(f.p); ok {
					return rep, nil
				}
			}
		}
	}

	if !*eng && *format == "csv" {
		fmt.Println(csvHeader)
	}

	var (
		w       *waterfall
		lastOut time.Time
	)
	if *wf {
		w = newWaterfall(prm)
		w.start()
		w.render()
	}
	quit := func() {
		if w != nil {
			w.stop()
		}
		cleanup()
		fmt.Println()
	}

	for {
		select {
		case <-sig:
			quit()
			return
		case <-winch:
			if w != nil {
				w.resize()
				w.render()
			}
		default:
		}

		rep, err := next()
		if err != nil {
			if soft(err) {
				continue
			}
			if w != nil {
				w.stop()
			}
			log.Fatal(err)
		}
		switch {
		case w != nil:
			if rep.eng {
				w.push(rep)
				w.render()
			}
		case *eng && rep.eng:
			renderEng(rep, prm)
		default:
			if now := time.Now(); now.Sub(lastOut) >= *interval {
				lastOut = now
				printBasic(rep, *format)
			}
		}
	}
}
