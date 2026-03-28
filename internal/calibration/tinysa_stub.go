//go:build !linux

package calibration

import "errors"

// TinySA wraps a tinySA Ultra serial connection.
type TinySA struct{}

// Open connects to the tinySA on portName (e.g. /dev/ttyACM0) at 115200 baud.
func Open(portName string) (*TinySA, error) {
	return nil, errors.New("tinySA serial not supported on this platform")
}

// Close sends "resume" (returns tinySA to normal sweep mode) then closes the port.
func (t *TinySA) Close() error { return nil }

// SetOutput commands the tinySA Ultra to emit CW at freqHz with levelDBm output power.
func (t *TinySA) SetOutput(_ int64, _ float64) error { return nil }
