package main

// Sensor configuration subcommands: set-gate and reset.

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"go.bug.st/serial"
)

const (
	cmdSetGate      = 0x0064
	cmdFactoryReset = 0x00A2
	cmdRestart      = 0x00A3
)

func openPort(name string, baud int) (serial.Port, *bufio.Reader, error) {
	port, err := serial.Open(name, &serial.Mode{BaudRate: baud})
	if err != nil {
		return nil, nil, err
	}
	if err := port.SetReadTimeout(100 * time.Millisecond); err != nil {
		port.Close()
		return nil, nil, err
	}
	return port, bufio.NewReader(portReader{port}), nil
}

func readParams(port serial.Port, r *bufio.Reader) (*params, error) {
	p, err := sendCmd(port, r, cmdReadParams, nil)
	if err != nil {
		return nil, err
	}
	return parseParams(p)
}

// gatePayload builds the 0x0064 value: three (word, uint32) pairs.
func gatePayload(gate, motion, static int) []byte {
	var v []byte
	for i, x := range [...]int{gate, motion, static} {
		v = binary.LittleEndian.AppendUint16(v, uint16(i))
		v = binary.LittleEndian.AppendUint32(v, uint32(x))
	}
	return v
}

func printParams(prm *params) {
	fmt.Println("gate  range        motion  static")
	for g := range prm.motionThr {
		fmt.Printf("%2d    %.2f-%.2fm  %6d  %6d\n", g,
			float64(g)*gateWidthM, float64(g+1)*gateWidthM, prm.motionThr[g], at(prm.staticThr, g))
	}
	fmt.Printf("no-one timeout=%ds\n", prm.noOneSec)
}

// setGate: mmwaver set-gate [-motion N] [-static N] <gate|all> [port]
func setGate(args []string, baud int) error {
	fs := flag.NewFlagSet("set-gate", flag.ExitOnError)
	motion := fs.Int("motion", -1, "motion threshold 0-100 (omit to keep current)")
	static := fs.Int("static", -1, "static threshold 0-100 (omit to keep current)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: mmwaver [-baud N] set-gate [-motion N] [-static N] <gate|all> [serial-port]")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	gate := -1 // all
	if g, err := strconv.Atoi(fs.Arg(0)); err == nil {
		gate = g
	} else if fs.Arg(0) != "all" {
		fs.Usage()
		os.Exit(2)
	}
	if gate < -1 || (*motion < 0 && *static < 0) || *motion > 100 || *static > 100 {
		fs.Usage()
		os.Exit(2)
	}

	port, r, err := openPort(resolvePort(fs.Arg(1)), baud)
	if err != nil {
		return err
	}
	defer port.Close()

	return withConfig(port, r, func() error {
		prm, err := readParams(port, r)
		if err != nil {
			return err
		}
		n := len(prm.motionThr)
		if gate >= n {
			return fmt.Errorf("gate %d out of range 0-%d", gate, n-1)
		}
		for g := range n {
			if gate >= 0 && g != gate {
				continue
			}
			m, s := at(prm.motionThr, g), at(prm.staticThr, g)
			if *motion >= 0 {
				m = *motion
			}
			if *static >= 0 {
				s = *static
			}
			if _, err := sendCmd(port, r, cmdSetGate, gatePayload(g, m, s)); err != nil {
				return fmt.Errorf("gate %d: %w", g, err)
			}
		}
		if prm, err = readParams(port, r); err != nil {
			return err
		}
		printParams(prm)
		return nil
	})
}

// factoryReset: mmwaver reset [port]. Restores defaults and restarts the module.
func factoryReset(args []string, baud int) error {
	fs := flag.NewFlagSet("reset", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, "usage: mmwaver [-baud N] reset [serial-port]") }
	fs.Parse(args)

	port, r, err := openPort(resolvePort(fs.Arg(0)), baud)
	if err != nil {
		return err
	}
	defer port.Close()

	if _, err := sendCmd(port, r, cmdEnableConfig, []byte{0x01, 0x00}); err != nil {
		return err
	}
	if _, err := sendCmd(port, r, cmdFactoryReset, nil); err != nil {
		return err
	}
	// Reset takes effect after restart; the reboot also leaves config mode,
	// so no end-config command is sent.
	if _, err := sendCmd(port, r, cmdRestart, nil); err != nil {
		return err
	}
	fmt.Println("factory defaults restored, module restarting")
	return nil
}
