package spectrumanalyzer

import (
	"log"
	"sync"
	"time"

	"github.com/konradit/michelada/internal/calibration"
)

// calFields groups calibration state embedded in Analyzer.
// Defined here rather than in analyzer.go to keep concerns separate.
type calFields struct {
	calMu sync.RWMutex
	cal   *calibration.CalTable
}

// SetCalibration installs a new calibration table.  Pass nil to revert to
// uncalibrated (dBfs) readings.  Safe to call while a sweep is running.
func (a *Analyzer) SetCalibration(ct *calibration.CalTable) {
	a.calMu.Lock()
	a.cal = ct
	a.calMu.Unlock()

	if ct != nil {
		log.Printf("[SA] calibration loaded: %d points (created %s)",
			len(ct.Points), ct.CreatedAt.Format(time.RFC3339))
	} else {
		log.Printf("[SA] calibration cleared")
	}
}

// LoadCalibration reads the default calibration file and installs it.
// Silently does nothing if the file does not exist.
func (a *Analyzer) LoadCalibration() {
	ct, err := calibration.Load(calibration.DefaultCalPath())
	if err != nil {
		log.Printf("[SA] cal load error: %v", err)

		return
	}

	a.SetCalibration(ct)
}

// CalInfo returns a brief status string: the creation timestamp of the loaded
// calibration, or "uncalibrated" if none is installed.
func (a *Analyzer) CalInfo() string {
	a.calMu.RLock()
	defer a.calMu.RUnlock()

	if a.cal == nil {
		return "uncalibrated"
	}

	return a.cal.CreatedAt.Format("2006-01-02 15:04:05")
}

// ApplyCalibration adds the interpolated offset to power (dBfs → dBm).
// Returns power unchanged if no calibration table is loaded.
func (a *Analyzer) ApplyCalibration(freqHz int64, power float32) float32 {
	return a.applyCalibration(freqHz, power)
}

// applyCalibration adds the interpolated offset to power (dBfs → dBm).
// Returns power unchanged if no calibration table is loaded.
func (a *Analyzer) applyCalibration(freqHz int64, power float32) float32 {
	a.calMu.RLock()
	ct := a.cal
	a.calMu.RUnlock()

	if ct == nil {
		return power
	}

	return power + float32(ct.Interpolate(freqHz))
}
