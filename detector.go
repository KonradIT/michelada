package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os/exec"
	"slices"
	"sync"
	"time"

	"github.com/konradit/michelada/internal/caribou"
)

// Detector — two-phase FPV video detection:
//
//	Phase 1: fast RSSI sweep across the full band to find power peaks
//	Phase 2: PAL video verification only on the peak frequencies
const (
	detectorDwell    = 3 * time.Second // PAL verification dwell time
	peakThresholdDB  = float32(10)     // dB above median to qualify as a peak
	detSettleSamples = 512             // 0.13 ms at 4 Msps
	detMeasSamples   = 1024            // 0.26 ms at 4 Msps
)

// detectorBand defines a continuous frequency range for RSSI sweeping.
type detectorBand struct {
	Name    string
	StartHz int64
	StopHz  int64
	StepHz  int64
}

// Sweep ranges per band — covers all possible FPV channels with margin.
var detectorSweepBands = map[string]detectorBand{
	"5_8": {Name: "5.8 GHz", StartHz: 5_640_000_000, StopHz: 5_945_000_000, StepHz: 5_000_000},
	"1_2": {Name: "1.2 GHz", StartHz: 1_060_000_000, StopHz: 1_380_000_000, StepHz: 5_000_000},
	"3_3": {Name: "3.3 GHz", StartHz: 2_980_000_000, StopHz: 4_960_000_000, StepHz: 10_000_000},
}

type detection struct {
	FreqHz int       `json:"freq_hz"`
	Time   time.Time `json:"time"`
	Name   string    `json:"name"`
}

var (
	detectorStopCh   chan struct{}
	detectorMu       sync.Mutex
	detectorScanning int    // current frequency (Hz), 0 if idle
	detectorPhase    string // "sweep" or "verify"
	detectorLog      []detection
	detectorLogMu    sync.Mutex
)

func startDetector() {
	detectorMu.Lock()
	defer detectorMu.Unlock()

	detectorStopCh = make(chan struct{})
	go detectorLoop(detectorStopCh)
}

func stopDetector() {
	detectorMu.Lock()
	defer detectorMu.Unlock()

	if detectorStopCh != nil {
		close(detectorStopCh)
		detectorStopCh = nil
	}

	caribou.StopHiF()

	detectorScanning = 0
	detectorPhase = ""
}

func detectorLoop(stop chan struct{}) {
	// Build sweep bands from config
	var bands []detectorBand

	for _, b := range appCfg.DetectorBands {
		def, ok := detectorSweepBands[b]
		if !ok {
			log.Printf("[detector] unknown band %q, skipping", b)

			continue
		}

		bands = append(bands, def)
	}

	if len(bands) == 0 {
		log.Printf("[detector] no bands configured, exiting")

		return
	}

	log.Printf("[detector] configured bands: %v", appCfg.DetectorBands)

	cooldown := time.Duration(appCfg.DetectorCooldown) * time.Second
	lastSeen := make(map[int64]time.Time)

	// Sampler for RSSI sweep (same pattern as REB)
	var (
		smu     sync.Mutex
		pending []float32
		notify  = make(chan struct{}, 1)
	)

	samplerCb := func(samples []float32) {
		smu.Lock()

		pending = append(pending, samples...)
		smu.Unlock()

		select {
		case notify <- struct{}{}:
		default:
		}
	}
	drain := func() {
		smu.Lock()
		pending = pending[:0]
		smu.Unlock()
	}
	waitN := func(n int) bool {
		for {
			smu.Lock()
			have := len(pending)
			smu.Unlock()

			if have >= n {
				return true
			}

			select {
			case <-stop:
				return false
			case <-notify:
			case <-time.After(500 * time.Millisecond):
			}
		}
	}

	for {
		for _, band := range bands {
			// ── Phase 1: fast RSSI sweep ────────────────────────
			setPhase("sweep")
			log.Printf("[detector] sweeping %s (%d-%d MHz, step %d MHz)",
				band.Name, band.StartHz/1e6, band.StopHz/1e6, band.StepHz/1e6)

			caribou.OnVideo = samplerCb

			caribou.SetHiFSampleRate(4_000_000)
			caribou.SetHiFGain(48)
			caribou.SetHiFAGC(false)
			caribou.SetHiFBandwidth(2_000_000)

			var (
				sweepFreqs  []int64
				sweepPowers []float32
			)

			for freq := band.StartHz; freq <= band.StopHz; freq += band.StepHz {
				select {
				case <-stop:
					caribou.StopHiF()

					caribou.OnVideo = nil

					setPhase("")

					return
				default:
				}

				setScanning(int(freq))

				// Stop→Start every step: recovers from PLL lock failures
				// (RFFC5072 dead zone or AT86RF215 IF out of range).
				caribou.StopHiF()
				caribou.StartHiF(int(freq))

				drain()

				if !waitN(detSettleSamples + detMeasSamples) {
					caribou.StopHiF()

					caribou.OnVideo = nil

					setPhase("")

					return
				}

				smu.Lock()

				n := min(len(pending), detMeasSamples)

				start := len(pending) - n

				var sum float32
				for _, v := range pending[start : start+n] {
					sum += v
				}

				pending = pending[:0]
				smu.Unlock()

				mean := float64(sum) / float64(n)
				if mean < 1e-12 {
					mean = 1e-12
				}

				powerDB := float32(20 * math.Log10(mean))

				sweepFreqs = append(sweepFreqs, freq)
				sweepPowers = append(sweepPowers, powerDB)
			}

			caribou.StopHiF()

			// ── Find peaks ──────────────────────────────────────
			peaks := findPeaks(sweepFreqs, sweepPowers, peakThresholdDB)
			if len(peaks) == 0 {
				log.Printf("[detector] %s: no peaks found", band.Name)

				continue
			}

			log.Printf("[detector] %s: %d peak(s) found: %v MHz",
				band.Name, len(peaks), freqsToMHz(peaks))

			// ── Phase 2: PAL verify each peak ───────────────────
			setPhase("verify")

			caribou.OnVideo = palVideoFeed

			for _, freq := range peaks {
				select {
				case <-stop:
					caribou.StopHiF()

					caribou.OnVideo = nil

					setPhase("")

					return
				default:
				}

				setScanning(int(freq))
				caribou.StopHiF()
				caribou.StartHiF(int(freq))

				select {
				case <-stop:
					caribou.StopHiF()

					caribou.OnVideo = nil

					setPhase("")

					return
				case <-time.After(detectorDwell):
				}

				if palDecoder.RealVideoProbe(detectorDwell) {
					now := time.Now()

					last, hasSeen := lastSeen[freq]
					if !hasSeen || now.Sub(last) >= cooldown {
						mhz := freq / 1_000_000
						log.Printf("[detector] VIDEO DETECTED at %d MHz (%d Hz)", mhz, freq)
						lastSeen[freq] = now

						d := detection{
							FreqHz: int(freq),
							Time:   now,
							Name:   fmt.Sprintf("%d MHz", mhz),
						}

						detectorLogMu.Lock()

						detectorLog = append(detectorLog, d)
						if len(detectorLog) > 100 {
							detectorLog = detectorLog[len(detectorLog)-100:]
						}
						detectorLogMu.Unlock()

						for _, script := range appCfg.Scripts.OnVideoDetected {
							go fireScript(script, int(freq))
						}
					} else {
						log.Printf("[detector] %d MHz: cooldown active (%v remaining)",
							freq/1_000_000, cooldown-now.Sub(last))
					}
				}
			}

			caribou.StopHiF()
		}
	}
}

// findPeaks returns the center frequency of each power bump above
// median + thresholdDB.  Consecutive elevated bins are grouped and
// the frequency with the highest power in each group is returned.
func findPeaks(freqs []int64, powers []float32, thresholdDB float32) []int64 {
	if len(powers) == 0 {
		return nil
	}

	// Median = noise floor estimate
	sorted := make([]float32, len(powers))
	copy(sorted, powers)
	slices.Sort(sorted)
	median := sorted[len(sorted)/2]
	threshold := median + thresholdDB

	var peaks []int64

	inPeak := false

	var (
		peakFreq  int64
		peakPower float32
	)

	for i, p := range powers {
		if p > threshold {
			if !inPeak {
				inPeak = true
				peakFreq = freqs[i]
				peakPower = p
			} else if p > peakPower {
				peakFreq = freqs[i]
				peakPower = p
			}
		} else {
			if inPeak {
				peaks = append(peaks, peakFreq)
				inPeak = false
			}
		}
	}

	if inPeak {
		peaks = append(peaks, peakFreq)
	}

	return peaks
}

func freqsToMHz(freqs []int64) []int64 {
	out := make([]int64, len(freqs))
	for i, f := range freqs {
		out[i] = f / 1_000_000
	}

	return out
}

func setScanning(freq int) {
	detectorMu.Lock()
	detectorScanning = freq
	detectorMu.Unlock()
}

func setPhase(p string) {
	detectorMu.Lock()
	detectorPhase = p
	detectorMu.Unlock()
}

func fireScript(script string, freqHz int) {
	full := fmt.Sprintf("%s --freq %d", script, freqHz)
	cmd := exec.Command("sh", "-c", full)
	log.Printf("[detector] exec: sh -c %q", full)

	err := cmd.Start()
	if err != nil {
		log.Printf("[detector] exec error: %v", err)
	}
}

// HTTP handlers

func detectorStatusHandler(w http.ResponseWriter, r *http.Request) {
	detectorMu.Lock()
	scanning := detectorScanning
	phase := detectorPhase
	detectorMu.Unlock()

	detectorLogMu.Lock()
	logCopy := make([]detection, len(detectorLog))
	copy(logCopy, detectorLog)
	detectorLogMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"active":        appCfg.DetectorStartOnBoot,
		"bands":         appCfg.DetectorBands,
		"scanning_freq": scanning,
		"phase":         phase,
		"detections":    logCopy,
	})
}

func detectorEnableHandler(w http.ResponseWriter, r *http.Request) {
	appCfg.DetectorStartOnBoot = true

	err := saveConfig()
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)

		return
	}

	log.Printf("[detector] enabled in config — restart required")
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"ok":true,"restart_required":true}`)
}

func detectorDisableHandler(w http.ResponseWriter, r *http.Request) {
	appCfg.DetectorStartOnBoot = false

	err := saveConfig()
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)

		return
	}

	log.Printf("[detector] disabled in config — restart required")
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"ok":true,"restart_required":true}`)
}
