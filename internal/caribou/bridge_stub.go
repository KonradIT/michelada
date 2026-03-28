//go:build !(linux && cgo)

// Package caribou provides a stub for the CaribouLite SDR bridge on
// platforms where the native library is unavailable.
package caribou

import "log"

// OnRSSI is called on each S1G RSSI measurement (rssi in dBm, freq in Hz).
// Set before calling StartS1G.
var OnRSSI func(rssi, freq float32)

// OnVideo is called with each batch of IQ power-envelope samples from the HiF radio.
// Set before calling StartHiF.
var OnVideo func([]float32)

// Info prints CaribouLite hardware information to stdout.
func Info() { log.Println("[caribou] stub: CaribouLite not available on this platform") }

// StartS1G begins RSSI scanning on the sub-1 GHz radio; OnRSSI is called per batch.
func StartS1G() {}

// StopS1G stops the S1G receiver.
func StopS1G() {}

// SetS1GFreq tunes the S1G radio to hz Hz.
func SetS1GFreq(_ int) {}

// SetS1GGain sets the S1G RX gain (0-63).
func SetS1GGain(_ int) {}

// SetS1GAGC enables or disables the S1G automatic gain control.
func SetS1GAGC(_ bool) {}

// StartHiF tunes the HiF radio to hz Hz and begins IQ-power-envelope receive.
func StartHiF(_ int) {}

// StopHiF stops the HiF receiver.
func StopHiF() {}

// SetHiFFreq retunes the HiF radio to hz Hz mid-stream.
func SetHiFFreq(_ int) {}

// SetHiFAGC enables or disables the HiF automatic gain control.
func SetHiFAGC(_ bool) {}

// SetHiFSampleRate sets the HiF ADC sample rate in Hz.
func SetHiFSampleRate(_ int) {}

// SetHiFBandwidth sets the HiF IF filter bandwidth in Hz.
func SetHiFBandwidth(_ int) {}

// SetHiFGain sets the HiF RX gain (0-63).
func SetHiFGain(_ int) {}
