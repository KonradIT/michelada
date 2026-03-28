package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"

	"github.com/konradit/michelada/internal/calibration"
)

// calState tracks an in-progress (or most-recently-completed) calibration run.
var calState struct {
	mu       sync.Mutex
	running  bool
	progress calibration.Progress // latest snapshot
	err      string               // non-empty on failure
	done     bool                 // true once the run finishes
}

// startCalibrationHandler — GET /calibrate/start
//
// Query parameters (all optional, with sensible defaults):
//
//	port   — tinySA serial port   (default: /dev/ttyACM0)
//	level  — output level in dBm  (default: -20)
//	start  — start frequency MHz  (default: 5725)
//	stop   — stop  frequency MHz  (default: 5825)
//	step   — step  frequency MHz  (default: 5)
func startCalibrationHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	port := q.Get("port")
	if port == "" {
		port = "/dev/ttyACM0"
	}

	level := -20.0

	if v := q.Get("level"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			level = f
		}
	}

	startMHz := int64(5725)

	if v := q.Get("start"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 100 && n <= 6000 {
			startMHz = n
		}
	}

	stopMHz := int64(5825)

	if v := q.Get("stop"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 100 && n <= 6000 {
			stopMHz = n
		}
	}

	stepMHz := int64(5)

	if v := q.Get("step"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 1 && n <= 100 {
			stepMHz = n
		}
	}

	calState.mu.Lock()
	if calState.running {
		calState.mu.Unlock()
		http.Error(w, `{"error":"calibration already running"}`, http.StatusConflict)

		return
	}

	calState.running = true
	calState.done = false
	calState.err = ""
	calState.progress = calibration.Progress{}
	calState.mu.Unlock()

	// Enter calibration mode: stops any active FPV or spectrum sweep.
	switchMode(modeCalibration)

	prog := make(chan calibration.Progress, 8)

	go func() {
		// Relay progress updates into calState while the run is active.
		var wg sync.WaitGroup
		wg.Add(1)

		go func() {
			defer wg.Done()

			for p := range prog {
				calState.mu.Lock()
				calState.progress = p
				calState.mu.Unlock()
			}
		}()

		ct, err := calibration.RunCalibration(
			port, level,
			startMHz*1_000_000, stopMHz*1_000_000, stepMHz*1_000_000,
			4_000_000, specConfig.Gain,
			prog,
		)
		close(prog)
		wg.Wait() // ensure last progress update lands in calState

		calState.mu.Lock()
		calState.running = false

		calState.done = true
		if err != nil {
			calState.err = err.Error()
			log.Printf("calibration error: %v", err)
		} else {
			log.Printf("calibration complete: %d points", len(ct.Points))
			specAnalyzer.SetCalibration(ct)
		}
		calState.mu.Unlock()

		// Return to normal spectrum operation.
		finishCalibration()
	}()

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true}`)
}

// calStatusHandler — GET /calibrate/status
//
// Returns JSON with fields:
//
//	running  bool              — true while a run is active
//	done     bool              — true once the run finishes (success or error)
//	error    string            — non-empty if the run failed
//	progress {Done,Total,...}  — latest progress snapshot
//	cal_info string            — creation timestamp of the loaded calibration, or ""
func calStatusHandler(w http.ResponseWriter, r *http.Request) {
	calState.mu.Lock()
	running := calState.running
	done := calState.done
	p := calState.progress
	errStr := calState.err
	calState.mu.Unlock()

	calInfo := ""
	if info := specAnalyzer.CalInfo(); info != "uncalibrated" {
		calInfo = info
	}

	w.Header().Set("Content-Type", "application/json")

	data, _ := json.Marshal(map[string]any{
		"running":  running,
		"done":     done,
		"error":    errStr,
		"progress": p,
		"cal_info": calInfo,
	})
	w.Write(data)
}
