//go:build linux

package calibration

// Serial port access implemented with stty + os.File so that no external
// module is required.  This only runs on Linux (Raspberry Pi), where stty
// and /dev/ttyACM* are always available.

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// TinySA wraps a tinySA Ultra serial connection.
type TinySA struct {
	f *os.File
}

// Open connects to the tinySA on portName (e.g. /dev/ttyACM0) at 115200 baud.
func Open(portName string) (*TinySA, error) {
	// O_NONBLOCK + O_NOCTTY on the initial open prevents blocking if the
	// serial driver has not yet asserted CLOCAL / carrier-detect.
	// We switch back to blocking mode immediately after so that VMIN/VTIME work.
	log.Printf("[CAL] Open: opening %s (O_NONBLOCK)", portName)
	f, err := os.OpenFile(portName, os.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", portName, err)
	}
	log.Printf("[CAL] Open: fd open, clearing O_NONBLOCK")
	if err := syscall.SetNonblock(int(f.Fd()), false); err != nil {
		f.Close()
		return nil, fmt.Errorf("setblocking %s: %w", portName, err)
	}

	// 115200 8N1, raw mode, no echo.
	// min 0 time 1 → VMIN=0, VTIME=1 (100 ms OS-level read timeout).
	log.Printf("[CAL] Open: running stty")
	out, err := exec.Command("stty", "-F", portName,
		"115200", "raw", "-echo", "cs8", "-cstopb", "-parenb",
		"min", "0", "time", "1",
	).CombinedOutput()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stty %s: %w: %s", portName, err, out)
	}
	log.Printf("[CAL] Open: stty done")

	t := &TinySA{f: f}
	t.drainFor(150 * time.Millisecond)
	log.Printf("[CAL] Open: ready")
	return t, nil
}

// Close sends "resume" (returns tinySA to normal sweep mode) then closes the port.
func (t *TinySA) Close() error {
	_ = t.sendCmd("resume")
	return t.f.Close()
}

// SetOutput commands the tinySA Ultra to emit CW at freqHz with levelDBm output power.
func (t *TinySA) SetOutput(freqHz int64, levelDBm float64) error {
	return t.sendCmd(fmt.Sprintf("output %d %.1f", freqHz, levelDBm))
}

// sendCmd writes a command terminated with CR+LF and waits for the "ch> " prompt.
func (t *TinySA) sendCmd(cmd string) error {
	if _, err := fmt.Fprintf(t.f, "%s\r\n", cmd); err != nil {
		return fmt.Errorf("tinySA write %q: %w", cmd, err)
	}
	return t.waitPrompt(3 * time.Second)
}

// waitPrompt reads until the 4-byte sequence "ch> " is found or the deadline expires.
func (t *TinySA) waitPrompt(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var acc []byte
	buf := make([]byte, 64)
	for time.Now().Before(deadline) {
		n, err := t.f.Read(buf)
		if n > 0 {
			acc = append(acc, buf[:n]...)
			if len(acc) >= 4 && string(acc[len(acc)-4:]) == "ch> " {
				return nil
			}
		}
		if err != nil && err != io.EOF {
			return fmt.Errorf("tinySA read: %w", err)
		}
	}
	return fmt.Errorf("tinySA: prompt timeout after %v", timeout)
}

// drainFor reads and discards all incoming bytes for the given duration.
func (t *TinySA) drainFor(dur time.Duration) {
	deadline := time.Now().Add(dur)
	buf := make([]byte, 64)
	for time.Now().Before(deadline) {
		t.f.Read(buf) //nolint:errcheck
	}
}
