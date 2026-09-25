package main

import (
	"bytes"
	"testing"
)

// Example from the LD2410 serial protocol manual: gate 3, motion 40, static 40.
func TestGatePayload(t *testing.T) {
	want := []byte{
		0x00, 0x00, 0x03, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x28, 0x00, 0x00, 0x00,
		0x02, 0x00, 0x28, 0x00, 0x00, 0x00,
	}
	if got := gatePayload(3, 40, 40); !bytes.Equal(got, want) {
		t.Fatalf("got % X\nwant % X", got, want)
	}
}
