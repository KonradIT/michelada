package calibration

// HiF gain calibration using a tinySA Ultra as a calibrated CW source.
//
// Procedure:
//   1. For each frequency step across the requested range:
//      a. Command tinySA to output CW at that frequency and a known level.
//      b. Tune the CaribouLite HiF radio to the same frequency.
//      c. Measure the mean power envelope (same method as sweepHiF).
//      d. offset = knownLevelDBm − measuredDBfs
//   2. Save the resulting offset table to ~/.config/michelada/hif_cal.json.
//
// Once loaded into the spectrum analyzer the offset is interpolated per-bin
// to convert the raw dBfs readings to calibrated dBm values.

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/konradit/michelada/internal/caribou"
)

// CalPoint records the measured dBfs→dBm offset at a single frequency.
type CalPoint struct {
	FreqHz   int64   `json:"freq_hz"`
	OffsetDB float64 `json:"offset_db"` // add this to raw dBfs to get dBm
}

// CalTable is a calibration table sorted ascending by FreqHz.
type CalTable struct {
	Points    []CalPoint `json:"points"`
	CreatedAt time.Time  `json:"created_at"`
}

// Interpolate returns the interpolated dBfs→dBm offset for freqHz.
// Returns 0 if the table is empty; clamps at the edges if out of range.
func (ct *CalTable) Interpolate(freqHz int64) float64 {
	pts := ct.Points
	if len(pts) == 0 {
		return 0
	}

	if freqHz <= pts[0].FreqHz {
		return pts[0].OffsetDB
	}

	if freqHz >= pts[len(pts)-1].FreqHz {
		return pts[len(pts)-1].OffsetDB
	}

	lo, hi := 0, len(pts)-1
	for lo+1 < hi {
		mid := (lo + hi) / 2
		if pts[mid].FreqHz <= freqHz {
			lo = mid
		} else {
			hi = mid
		}
	}

	t := float64(freqHz-pts[lo].FreqHz) / float64(pts[hi].FreqHz-pts[lo].FreqHz)

	return pts[lo].OffsetDB + t*(pts[hi].OffsetDB-pts[lo].OffsetDB)
}

// DefaultCalPath returns the default calibration file path.
func DefaultCalPath() string {
	home, _ := os.UserHomeDir()

	return filepath.Join(home, ".config", "michelada", "hif_cal.json")
}

// Load reads a CalTable from path.  Returns nil, nil if the file does not exist.
func Load(path string) (*CalTable, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	var ct CalTable
	if err := json.Unmarshal(data, &ct); err != nil {
		return nil, fmt.Errorf("parse cal file: %w", err)
	}

	sort.Slice(ct.Points, func(i, j int) bool {
		return ct.Points[i].FreqHz < ct.Points[j].FreqHz
	})

	return &ct, nil
}

// Progress is sent on the progress channel during RunCalibration.
type Progress struct {
	Done     int     `json:"Done"`
	Total    int     `json:"Total"`
	FreqHz   int64   `json:"FreqHz"`
	OffsetDB float64 `json:"OffsetDB"`
}

const (
	calSettleSamples  = 2048
	calMeasureSamples = 8192
)

// RunCalibration performs a gain calibration sweep using a tinySA Ultra as the
// CW reference source.  It takes exclusive ownership of the CaribouLite HiF
// radio for the duration; the caller must stop any competing HiF user (spectrum
// analyzer, PAL decoder) before calling.
//
//   - portName   — serial port for tinySA (e.g. /dev/ttyACM0)
//   - levelDBm   — tinySA output power, e.g. -20.0
//   - startHz    — first calibration frequency in Hz
//   - stopHz     — last  calibration frequency in Hz
//   - stepHz     — frequency increment in Hz
//   - sampleRate — caribou HiF sample rate in Hz (e.g. 4_000_000)
//   - gain       — caribou HiF gain (0-63)
//   - prog       — optional; progress updates are sent non-blocking; may be nil
//
// The completed CalTable is saved to DefaultCalPath() before returning.
func RunCalibration(
	portName string,
	levelDBm float64,
	startHz, stopHz, stepHz int64,
	sampleRate, gain int,
	prog chan<- Progress,
) (*CalTable, error) {
	log.Printf("[CAL] opening tinySA on %s", portName)

	tsa, err := Open(portName)
	if err != nil {
		return nil, err
	}
	defer tsa.Close()

	log.Printf("[CAL] tinySA open OK")

	sp := &sampler{ch: make(chan struct{}, 1)}
	caribou.OnVideo = sp.callback

	caribou.SetHiFSampleRate(sampleRate)
	caribou.SetHiFGain(gain)
	caribou.SetHiFAGC(false)
	caribou.SetHiFBandwidth(2_000_000)
	log.Printf("[CAL] caribou configured: rate=%d gain=%d", sampleRate, gain)

	var points []CalPoint

	total := int((stopHz-startHz)/stepHz) + 1
	log.Printf("[CAL] sweep: %.0fâ%.0f MHz step %.0f MHz  (%d points)",
		float64(startHz)/1e6, float64(stopHz)/1e6, float64(stepHz)/1e6, total)

	for i, freq := 0, startHz; freq <= stopHz; i, freq = i+1, freq+stepHz {
		// Tune radio
		log.Printf("[CAL] step %d/%d  freq=%.0f MHz", i+1, total, float64(freq)/1e6)

		if freq == startHz {
			caribou.StartHiF(int(freq))
		} else {
			caribou.SetHiFFreq(int(freq))
		}

		log.Printf("[CAL] radio tuned, sending tinySA output command")
		// Command tinySA to emit CW at this frequency
		err := tsa.SetOutput(freq, levelDBm)
		if err != nil {
			caribou.StopHiF()

			caribou.OnVideo = nil

			return nil, fmt.Errorf("tinySA tune at %d Hz: %w", freq, err)
		}

		log.Printf("[CAL] tinySA output set OK")
		// Extra settle: tinySA needs ~80 ms to stabilise phase-lock
		time.Sleep(80 * time.Millisecond)

		// Discard PLL settle samples
		sp.drain()
		log.Printf("[CAL] waiting for settle samples...")

		err = sp.wait(calSettleSamples, 2*time.Second)
		if err != nil {
			caribou.StopHiF()

			caribou.OnVideo = nil

			return nil, fmt.Errorf("settle timeout at %d Hz: %w", freq, err)
		}

		sp.drain()
		log.Printf("[CAL] settle done, collecting measure samples...")

		// Collect measurement samples
		err = sp.wait(calMeasureSamples, 2*time.Second)
		if err != nil {
			caribou.StopHiF()

			caribou.OnVideo = nil

			return nil, fmt.Errorf("measure timeout at %d Hz: %w", freq, err)
		}

		log.Printf("[CAL] measure done")

		raw := sp.take(calMeasureSamples)

		m := max(meanF32(raw), 1e-12)

		dBfs := 20 * math.Log10(float64(m))
		offset := levelDBm - dBfs

		points = append(points, CalPoint{FreqHz: freq, OffsetDB: offset})
		sendProgress(prog, Progress{
			Done: i + 1, Total: total, FreqHz: freq, OffsetDB: offset,
		})
	}

	caribou.StopHiF()

	caribou.OnVideo = nil

	ct := &CalTable{Points: points, CreatedAt: time.Now()}

	path := DefaultCalPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return ct, fmt.Errorf("mkdir cal dir: %w", err)
	}

	data, _ := json.MarshalIndent(ct, "", "  ")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return ct, fmt.Errorf("write cal file: %w", err)
	}

	return ct, nil
}

// ── internal helpers ──────────────────────────────────────────────────────────

type sampler struct {
	mu      sync.Mutex
	pending []float32
	ch      chan struct{}
}

func (s *sampler) callback(samples []float32) {
	s.mu.Lock()
	s.pending = append(s.pending, samples...)
	s.mu.Unlock()

	select {
	case s.ch <- struct{}{}:
	default:
	}
}

func (s *sampler) drain() {
	s.mu.Lock()
	s.pending = s.pending[:0]
	s.mu.Unlock()
}

func (s *sampler) wait(n int, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	for {
		s.mu.Lock()
		have := len(s.pending)
		s.mu.Unlock()

		if have >= n {
			return nil
		}

		select {
		case <-s.ch:
		case <-deadline.C:
			return fmt.Errorf("timeout waiting for %d samples", n)
		}
	}
}

func (s *sampler) take(n int) []float32 {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.pending) < n {
		n = len(s.pending)
	}

	out := make([]float32, n)
	copy(out, s.pending[:n])
	s.pending = s.pending[:0]

	return out
}

func meanF32(s []float32) float32 {
	if len(s) == 0 {
		return 0
	}

	var sum float32
	for _, v := range s {
		sum += v
	}

	return sum / float32(len(s))
}

func sendProgress(ch chan<- Progress, p Progress) {
	if ch == nil {
		return
	}

	select {
	case ch <- p:
	default:
	}
}
