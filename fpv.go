package main

// Pure-Go FPV layer.  All CGo calls are in internal/caribou (single import "C" package);
// this file contains only Go logic: channel table, HTTP handlers, and the
// startFPV / stopFPV / setFPVChannel helpers that delegate to the caribou package.

import (
	"fmt"
	"log"
	"net/http"
	"strconv"

	"github.com/konradit/michelada/internal/caribou"
)

// FPVChannels maps community channel names to carrier frequencies in Hz.
// Covers Raceband, Band A/B/E and Fat Shark (F) band.
var FPVChannels = map[string]int{
	// Raceband
	"R1": 5658000000, "R2": 5695000000, "R3": 5732000000, "R4": 5769000000,
	"R5": 5806000000, "R6": 5843000000, "R7": 5880000000, "R8": 5917000000,
	// Band A
	"A1": 5865000000, "A2": 5845000000, "A3": 5825000000, "A4": 5805000000,
	"A5": 5785000000, "A6": 5765000000, "A7": 5745000000, "A8": 5725000000,
	// Band B (Boscam)
	"B1": 5733000000, "B2": 5752000000, "B3": 5771000000, "B4": 5790000000,
	"B5": 5809000000, "B6": 5828000000, "B7": 5847000000, "B8": 5866000000,
	// Band E
	"E1": 5705000000, "E2": 5685000000, "E3": 5665000000, "E4": 5645000000,
	"E5": 5885000000, "E6": 5905000000, "E7": 5925000000, "E8": 5945000000,
	// Fat Shark / F band
	"F1": 5740000000, "F2": 5760000000, "F3": 5780000000, "F4": 5800000000,
	"F5": 5820000000, "F6": 5840000000, "F7": 5860000000, "F8": 5880000000,

	// 3.3 GHz bands (community naming; actual range 3.0-4.938 GHz)
	// Band A
	"3A1": 3000000000, "3A2": 3030000000, "3A3": 3060000000, "3A4": 3090000000,
	"3A5": 3120000000, "3A6": 3150000000, "3A7": 3180000000, "3A8": 3210000000,
	// Band B
	"3B1": 3240000000, "3B2": 3270000000, "3B3": 3300000000, "3B4": 3330000000,
	"3B5": 3370000000, "3B6": 3400000000, "3B7": 3430000000, "3B8": 3470000000,
	// Band E
	"3E1": 3500000000, "3E2": 3530000000, "3E3": 3560000000, "3E4": 3590000000,
	"3E5": 3620000000, "3E6": 3650000000, "3E7": 3680000000, "3E8": 3710000000,
	// Band F
	"3F1": 3740000000, "3F2": 3770000000, "3F3": 3800000000, "3F4": 3830000000,
	"3F5": 3860000000, "3F6": 3890000000, "3F7": 3920000000, "3F8": 3950000000,
	// Band R
	"3R1": 3980000000, "3R2": 4010000000, "3R3": 4040000000, "3R4": 4070000000,
	"3R5": 4100000000, "3R6": 4130000000, "3R7": 4160000000, "3R8": 4190000000,
	// Band P
	"3P1": 4220000000, "3P2": 4250000000, "3P3": 4280000000, "3P4": 4310000000,
	"3P5": 4340000000, "3P6": 4370000000, "3P7": 4400000000, "3P8": 4430000000,
	// Band H
	"3H1": 4470000000, "3H2": 4500000000, "3H3": 4530000000, "3H4": 4560000000,
	"3H5": 4590000000, "3H6": 4620000000, "3H7": 4650000000, "3H8": 4680000000,
	// Band U
	"3U1": 4710000000, "3U2": 4740000000, "3U3": 4770000000, "3U4": 4812000000,
	"3U5": 4839000000, "3U6": 4872000000, "3U7": 4911000000, "3U8": 4938000000,

	// 1.2 GHz band
	"L1": 1080000000, "L2": 1120000000, "L3": 1160000000, "L4": 1200000000,
	"L5": 1240000000, "L6": 1280000000, "L7": 1320000000, "L8": 1360000000,
	"L9": 1258000000,
}

// startFPV tunes the HiF radio to the named channel and begins FM-demod receive.
func startFPV(channelName string) {
	freq, ok := FPVChannels[channelName]
	if !ok {
		freq = 5800000000

		log.Printf("FPV: unknown channel %q, defaulting to 5800 MHz", channelName)
	}

	log.Printf("FPV: starting receive on %s (%d MHz)", channelName, freq/1_000_000)
	caribou.StartHiF(freq)
}

// stopFPV stops the HiF receiver.
func stopFPV() { caribou.StopHiF() }

// setFPVChannel retunes the HiF radio to a named channel.
//
// Uses a full Stop→Start cycle rather than SetHiFFreq to guarantee recovery
// from any RFFC5072 PLL deactivation caused by an out-of-range frequency
// (e.g. 3-5 GHz band attempts that fail PLL lock).  caribou.OnVideo is
// preserved across the restart because it is only cleared by the spectrum
// analyzer's Stop(), not by stopFPV/startFPV.
func setFPVChannel(channelName string) {
	freq, ok := FPVChannels[channelName]
	if !ok {
		log.Printf("FPV: unknown channel %q", channelName)

		return
	}

	log.Printf("FPV: switching to %s (%d MHz)", channelName, freq/1_000_000)
	caribou.StopHiF()
	caribou.StartHiF(freq)
}

// serveFPVChannels — GET /fpv/channels — returns a JSON object of name→MHz.
func serveFPVChannels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("{"))

	first := true
	for name, freq := range FPVChannels {
		if !first {
			w.Write([]byte(","))
		}

		w.Write([]byte(`"` + name + `":` + freqMHz(freq)))

		first = false
	}

	w.Write([]byte("}"))
}

// setFPVChannelHandler — POST/GET /fpv/channel?ch=R1 — switches the channel.
func setFPVChannelHandler(w http.ResponseWriter, r *http.Request) {
	ch := r.URL.Query().Get("ch")
	if ch == "" {
		http.Error(w, `{"error":"missing ch parameter"}`, http.StatusBadRequest)

		return
	}

	if _, ok := FPVChannels[ch]; !ok {
		http.Error(w, `{"error":"unknown channel"}`, http.StatusBadRequest)

		return
	}

	setFPVChannel(ch)
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// setFPVFreqHandler — GET /fpv/freq?mhz=N — tunes the HiF radio to an arbitrary
// frequency (100-6000 MHz) without requiring a named channel entry.
func setFPVFreqHandler(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query().Get("mhz")
	if v == "" {
		http.Error(w, `{"error":"missing mhz parameter"}`, http.StatusBadRequest)

		return
	}

	mhz, err := strconv.Atoi(v)
	if err != nil || mhz < 100 || mhz > 6000 {
		http.Error(w, `{"error":"mhz must be 100-6000"}`, http.StatusBadRequest)

		return
	}

	hz := mhz * 1_000_000
	log.Printf("FPV: manual tune to %d MHz", mhz)
	caribou.StopHiF()
	caribou.StartHiF(hz)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"mhz":%d}`, mhz)
}

// freqMHz converts a frequency in Hz to its MHz integer string representation.
func freqMHz(hz int) string {
	n := hz / 1_000_000
	if n == 0 {
		return "0"
	}

	digits := make([]byte, 0, 8)
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}

	return string(digits)
}
