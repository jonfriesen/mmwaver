package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"go.bug.st/serial"
	"golang.org/x/term"
)

var errNoPorts = errors.New("no serial ports found")

// pickPort shows an arrow-key menu of serial ports and returns the chosen one.
// Returns "" if the user cancels (q, Esc, Ctrl-C).
func pickPort() (string, error) {
	all, err := serial.GetPortsList()
	if err != nil {
		return "", err
	}
	var ports []string
	for _, p := range all {
		if strings.HasPrefix(p, "/dev/tty.") { // macOS: cu.* is the same device
			continue
		}
		ports = append(ports, p)
	}
	if len(ports) == 0 {
		return "", errNoPorts
	}

	cur := 0
	for i, p := range ports { // default to the first USB adapter
		if strings.Contains(strings.ToLower(p), "usb") {
			cur = i
			break
		}
	}

	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return "", err
	}
	defer term.Restore(fd, old)

	os.Stdout.WriteString("\033[?25l")
	defer os.Stdout.WriteString("\033[?25h")
	draw := func() {
		var sb strings.Builder
		sb.WriteString("Select a serial port (↑/↓ or j/k, Enter to confirm, q to quit)\r\n")
		for i, p := range ports {
			if i == cur {
				fmt.Fprintf(&sb, "\033[7m > %s \033[0m\r\n", p)
			} else {
				fmt.Fprintf(&sb, "   %s\r\n", p)
			}
		}
		fmt.Fprintf(&sb, "\033[%dA", len(ports)+1) // back to top for redraw
		os.Stdout.WriteString(sb.String())
	}
	done := func() { fmt.Printf("\033[%dB\r\n", len(ports)+1) }

	buf := make([]byte, 8)
	for {
		draw()
		n, err := os.Stdin.Read(buf)
		if err != nil {
			done()
			return "", err
		}
		switch k := string(buf[:n]); k {
		case "\033[A", "k":
			cur = (cur + len(ports) - 1) % len(ports)
		case "\033[B", "j":
			cur = (cur + 1) % len(ports)
		case "\r", "\n":
			done()
			return ports[cur], nil
		case "q", "\033", "\x03":
			done()
			return "", nil
		}
	}
}
