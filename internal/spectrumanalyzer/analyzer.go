package spectrumanalyzer

import (
	"log"
	"math"
	"sync"
	"time"

	"github.com/konradit/michelada/internal/caribou"
)

// Verbose controls whether detailed sweep logs are printed.
var Verbose bool

// Radio selects which CaribouLite radio to use for the sweep.
type Radio int

const (
	RadioHiF Radio = iota
	RadioS1G
)

// Config describes a spectrum sweep.
type Config struct {
	Radio          Radio
	StartFreq      int64 // Hz  (HiF: 100M-6G; S1G: 389.5M-510M or 779M-1020M)
	StopFreq       int64 // Hz
	AGC            bool
	Gain           int // 0-63
	SampleRate     int // Hz for HiF (AT86RF215 steps); ignored for S1G
	FFTSize        int // power of 2 → RBW = SampleRate / FFTSize
	StepSize       int // Hz — HiF frequency increment per scan step (default 500 kHz)
	SettleSamples  int // samples discarded after retune for PLL lock (default 2048)
	MeasureSamples int // samples averaged per step for power estimate (default 4096)
}

// PowerBin is one frequency bucket in a sweep result.
type PowerBin struct {
	Freq  float64 `json:"f"` // Hz
	Power float32 `json:"p"` // dBfs
}

// Analyzer runs a continuous spectrum sweep and fans results out to subscribers.
type Analyzer struct {
	mu      sync.Mutex
	cfg     Config
	running bool
	stopCh  chan struct{}

	clientsMu sync.RWMutex
	clients   map[chan []PowerBin]bool

	// pending accumulates HiF power-envelope samples between callbacks
	pending   []float32
	pendingMu sync.Mutex
	pendingCh chan struct{} // signals new samples available

	calFields // calibration offset table (dBfs → dBm)
}

// New creates an idle Analyzer.
func New() *Analyzer {
	return &Analyzer{
		clients:   make(map[chan []PowerBin]bool),
		pendingCh: make(chan struct{}, 1),
	}
}

// Subscribe registers ch to receive completed sweeps.  ch must be buffered.
func (a *Analyzer) Subscribe(ch chan []PowerBin) {
	a.clientsMu.Lock()
	a.clients[ch] = true
	a.clientsMu.Unlock()
}

// Unsubscribe removes ch from the subscriber list.
func (a *Analyzer) Unsubscribe(ch chan []PowerBin) {
	a.clientsMu.Lock()
	delete(a.clients, ch)
	a.clientsMu.Unlock()
}

// Start begins a sweep with the given config.  Stops any running sweep first.
// Caller must have already stopped any PAL/FPV receiver that uses the same radio.
func (a *Analyzer) Start(cfg Config) {
	a.Stop()

	if !isPow2(cfg.FFTSize) || cfg.FFTSize < 64 {
		cfg.FFTSize = 1024
	}

	a.mu.Lock()
	a.cfg = cfg
	a.stopCh = make(chan struct{})
	a.running = true
	a.mu.Unlock()

	if cfg.Radio == RadioHiF {
		go a.sweepHiF(cfg, a.stopCh)
	} else {
		go a.sweepS1G(cfg, a.stopCh)
	}
}

// Stop halts the current sweep and deregisters HiF/S1G callbacks.
func (a *Analyzer) Stop() {
	a.mu.Lock()
	if !a.running {
		a.mu.Unlock()

		return
	}

	close(a.stopCh)
	a.running = false
	a.mu.Unlock()

	// Clear callbacks so stray samples don't arrive after stop
	caribou.OnVideo = nil
	caribou.OnRSSI = nil

	time.Sleep(20 * time.Millisecond) // let in-flight callbacks drain
}

// Running reports whether a sweep is active.
func (a *Analyzer) Running() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.running
}

// Config returns the current sweep config.
func (a *Analyzer) Config() Config {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.cfg
}

// broadcast sends a completed sweep to all subscribers (non-blocking).
func (a *Analyzer) broadcast(bins []PowerBin) {
	a.clientsMu.RLock()

	for ch := range a.clients {
		select {
		case ch <- bins:
		default:
		}
	}

	a.clientsMu.RUnlock()
}

// HiF sweep

const (
	// DefaultHiFStepHz is the default frequency increment per scan step.
	// 500 kHz gives good resolution for FPV-band scanning; the IF filter
	// (2 MHz wide) acts as the effective RBW.
	DefaultHiFStepHz = 500_000

	// DefaultSettleSamples: samples to discard after retuning (PLL re-lock time).
	// At 4 Msps, 2048 samples ≈ 0.5 ms — enough for the AT86RF215 VCO to settle.
	DefaultSettleSamples = 2048

	// DefaultMeasureSamples: samples averaged for the power estimate per step.
	// 4096 samples at 4 Msps = 1 ms dwell — smooth without being too slow.
	DefaultMeasureSamples = 4096
)

// sweepHiF scans the HiF radio across the configured frequency range using the
// power-envelope approach: tune → settle → average mean(envelope) → one bin.
//
// This is analogous to the S1G RSSI scan.  An FFT of the power envelope is NOT
// used because the HiF delivers a scalar magnitude (AM detector output), not IQ
// samples; FFT-ing that signal produces characteristic sawtooth artifacts rather
// than the RF power spectrum.
func (a *Analyzer) sweepHiF(cfg Config, stop chan struct{}) {
	caribou.SetHiFSampleRate(cfg.SampleRate)
	caribou.SetHiFGain(cfg.Gain)
	caribou.SetHiFAGC(cfg.AGC)
	// Wide IF window: 2 MHz = maximum — integrates the most signal power per step.
	caribou.SetHiFBandwidth(2_000_000)

	// Install the sample callback once; it stays for the lifetime of the sweep.
	caribou.OnVideo = func(samples []float32) {
		a.pendingMu.Lock()
		a.pending = append(a.pending, samples...)
		a.pendingMu.Unlock()

		select {
		case a.pendingCh <- struct{}{}:
		default:
		}
	}

	stepHz := int64(cfg.StepSize)
	if stepHz <= 0 {
		stepHz = DefaultHiFStepHz
	}

	settleSamples := cfg.SettleSamples
	if settleSamples <= 0 {
		settleSamples = DefaultSettleSamples
	}

	measureSamples := cfg.MeasureSamples
	if measureSamples <= 0 {
		measureSamples = DefaultMeasureSamples
	}

	for {
		var sweep []PowerBin

		for freq := cfg.StartFreq; freq <= cfg.StopFreq; freq += stepHz {
			select {
			case <-stop:
				caribou.StopHiF()

				return
			default:
			}

			if freq == cfg.StartFreq {
				caribou.StartHiF(int(freq))
			} else {
				caribou.SetHiFFreq(int(freq))
			}

			// Discard settle samples
			a.pendingMu.Lock()
			a.pending = a.pending[:0]
			a.pendingMu.Unlock()

			err := a.waitSamples(settleSamples, stop)
			if err != nil {
				caribou.StopHiF()

				return
			}

			a.pendingMu.Lock()
			a.pending = a.pending[:0]
			a.pendingMu.Unlock()

			// Collect measurement samples
			err = a.waitSamples(measureSamples, stop)
			if err != nil {
				caribou.StopHiF()

				return
			}

			a.pendingMu.Lock()

			var sum float32

			n := min(len(a.pending), measureSamples)

			for _, v := range a.pending[:n] {
				sum += v
			}

			a.pending = a.pending[:0]
			a.pendingMu.Unlock()

			// Mean envelope → dBfs
			mean := float64(sum) / float64(n)
			if mean < 1e-12 {
				mean = 1e-12
			}

			powerDB := float32(20 * math.Log10(mean))
			powerDB = a.applyCalibration(freq, powerDB)
			sweep = append(sweep, PowerBin{Freq: float64(freq), Power: powerDB})
		}

		if len(sweep) > 0 {
			if Verbose {
				log.Printf("[SA] HiF scan: %d bins  %.0f-%.0f MHz",
					len(sweep), float64(cfg.StartFreq)/1e6, float64(cfg.StopFreq)/1e6)
			}
			a.broadcast(sweep)
		}
	}
}

// waitSamples blocks until at least n samples are pending or stop fires.
func (a *Analyzer) waitSamples(n int, stop chan struct{}) error {
	for {
		a.pendingMu.Lock()
		have := len(a.pending)
		a.pendingMu.Unlock()

		if have >= n {
			return nil
		}

		select {
		case <-stop:
			return errStopped
		case <-a.pendingCh:
		case <-time.After(500 * time.Millisecond):
			// timeout guard — continue to recheck
		}
	}
}

var errStopped = &stoppedErr{}

type stoppedErr struct{}

func (e *stoppedErr) Error() string { return "analyzer stopped" }

// S1G sweep (RSSI-based, no FFT needed)

const s1gDwellMs = 80 // ms to dwell per frequency step

func (a *Analyzer) sweepS1G(cfg Config, stop chan struct{}) {
	stepHz := int64(500_000) // 500 kHz steps — S1G RSSI is scalar, finer than FFT needed

	// Collect one RSSI measurement then advance
	type rssiSample struct {
		rssi float32
		freq float32
	}

	rssiCh := make(chan rssiSample, 8)
	caribou.OnRSSI = func(rssi, freq float32) {
		select {
		case rssiCh <- rssiSample{rssi, freq}:
		default:
		}
	}

	caribou.SetS1GGain(cfg.Gain)
	caribou.SetS1GAGC(cfg.AGC)
	caribou.StartS1G()

	defer caribou.StopS1G()

	for {
		var sweep []PowerBin

		for freq := cfg.StartFreq; freq <= cfg.StopFreq; freq += stepHz {
			select {
			case <-stop:
				return
			default:
			}

			caribou.SetS1GFreq(int(freq))

			// Drain stale samples and wait for a fresh one
			deadline := time.After(time.Duration(s1gDwellMs) * time.Millisecond)

			var lastRSSI float32 = -120

		drain:
			for {
				select {
				case s := <-rssiCh:
					lastRSSI = s.rssi
				case <-deadline:
					break drain
				case <-stop:
					return
				}
			}

			sweep = append(sweep, PowerBin{Freq: float64(freq), Power: lastRSSI})
		}

		if len(sweep) > 0 {
			if Verbose {
				log.Printf("[SA] S1G sweep: %d bins  %.1f-%.1f MHz",
					len(sweep), float64(cfg.StartFreq)/1e6, float64(cfg.StopFreq)/1e6)
			}
			a.broadcast(sweep)
		}
	}
}

// Utils

// isPow2 returns true if n is a positive power of 2.
func isPow2(n int) bool {
	return n > 0 && n&(n-1) == 0 //nolint:gocritic
}
