//go:build linux && cgo

package caribou

// #include <stdint.h>
// #include "cariboubridge.h"
// #cgo CFLAGS: -I/usr/include/cariboulite
// #cgo LDFLAGS: -lcariboulite -lstdc++
import "C"
import (
	"log"
	"time"
	"unsafe"
)

// OnRSSI is called on each S1G RSSI measurement (rssi in dBm, freq in Hz).
// Set before calling StartS1G.
var OnRSSI func(rssi, freq float32)

// OnVideo is called with each batch of IQ power-envelope samples from the HiF radio.
// Set before calling StartHiF.
var OnVideo func([]float32)

var (
	videoTotal   int64
	videoLastLog time.Time
)

//export receiveDataGateway
func receiveDataGateway(rssi C.float, freq C.float) {
	cb := OnRSSI
	if cb != nil {
		cb(float32(rssi), float32(freq))
	}
}

//export receiveVideoGateway
func receiveVideoGateway(samples *C.float, numSamples C.int) {
	count := int(numSamples)
	cb := OnVideo // snapshot to avoid race with nil assignment
	if count == 0 || samples == nil || cb == nil {
		return
	}
	videoTotal += int64(count)
	if now := time.Now(); now.Sub(videoLastLog) >= 3*time.Second {
		log.Printf("[bridge] HiF→Go: %d total samples delivered", videoTotal)
		videoLastLog = now
	}
	cSlice := (*[1 << 28]float32)(unsafe.Pointer(samples))[:count:count]
	buf := make([]float32, count)
	copy(buf, cSlice)
	cb(buf)
}

// Info prints CaribouLite hardware information to stdout.
func Info() { C.info() }

// StartS1G begins RSSI scanning on the sub-1 GHz radio; OnRSSI is called per batch.
func StartS1G() { C.readRssi(C.ReceiveData(C.receiveDataGateway)) }

// StopS1G stops the S1G receiver.
func StopS1G() { C.stopRssi() }

// SetS1GFreq tunes the S1G radio to hz Hz.
func SetS1GFreq(hz int) { C.setFreq(C.int(hz)) }

// SetS1GGain sets the S1G RX gain (0-63).
func SetS1GGain(gain int) { C.setRxGain(C.int(gain)) }

// SetS1GAGC enables or disables the S1G automatic gain control.
func SetS1GAGC(agc bool) {
	C.setAgc(*(*C._Bool)(unsafe.Pointer(&agc)))
}

// StartHiF tunes the HiF radio to hz Hz and begins IQ-power-envelope receive.
// hz must be passed as C.longlong — 5.8 GHz values overflow C.int (32-bit).
func StartHiF(hz int) {
	C.startHifFpv(C.double(hz), C.ReceiveVideoSamples(C.receiveVideoGateway))
}

// StopHiF stops the HiF receiver.
func StopHiF() { C.stopHifFpv() }

// SetHiFFreq retunes the HiF radio to hz Hz mid-stream.
func SetHiFFreq(hz int) { C.setHifFreq(C.double(hz)) }

// SetHiFAGC enables or disables the HiF automatic gain control.
func SetHiFAGC(agc bool) { C.setHifAgc(*(*C._Bool)(unsafe.Pointer(&agc))) }

// SetHiFSampleRate sets the HiF ADC sample rate in Hz.
// AT86RF215 valid steps: 400k, 500k, 666k, 800k, 1M, 1.33M, 2M, 4M sps.
// SDK rounds to nearest valid step.
func SetHiFSampleRate(hz int) { C.setHifSampleRate(C.double(hz)) }

// SetHiFBandwidth sets the HiF IF filter bandwidth in Hz (160 000 - 2 000 000).
// The SDK rounds to the nearest AT86RF215 step. Narrower bandwidth sharpens the
// slope-detection power contrast but may cut into the baseband video signal.
func SetHiFBandwidth(hz int) { C.setHifBandwidth(C.double(hz)) }

// SetHiFGain sets the HiF RX gain (0-63). Default used at start is 48.
func SetHiFGain(gain int) { C.setHifGain(C.int(gain)) }
