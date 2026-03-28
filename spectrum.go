package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/konradit/michelada/internal/caribou"
	"github.com/konradit/michelada/internal/spectrumanalyzer"
)

// Mode management — HiF radio is shared between PAL decoder and spectrum analyzer

type appMode int

const (
	modeFPV         appMode = iota
	modeSpectrum            // default at startup
	modeCalibration         // HiF owned exclusively by the calibration routine
	modeREB                 // REB check for jammers and other EW
	modeDetector            // FPV video detector — scans bands, fires scripts
)

var (
	currentMode   = modeSpectrum
	modeMu        sync.Mutex
	activeFPVChan = "R6"

	// palVideoFeed is set by main() to decoder.Feed so switchMode can restore
	// caribou.OnVideo when transitioning back to FPV mode.
	palVideoFeed func([]float32)

	specAnalyzer = spectrumanalyzer.New()
	specConfig   = spectrumanalyzer.Config{
		Radio:      spectrumanalyzer.RadioHiF,
		StartFreq:  5_725_000_000,
		StopFreq:   5_825_000_000,
		AGC:        false,
		Gain:       48,
		SampleRate: 4_000_000,
		FFTSize:    1024,
	}
)

// switchMode transitions between spectrum and FPV modes.
// Stops the current mode's radio activity before starting the next.
// Calls originating from modeCalibration→spectrum use finishCalibration instead.
func switchMode(m appMode) {
	modeMu.Lock()
	defer modeMu.Unlock()

	if m == currentMode {
		return
	}
	// Calibration and detector own the radio exclusively; block external mode changes.
	if currentMode == modeCalibration {
		log.Printf("switchMode(%v) blocked: calibration in progress", m)

		return
	}

	if currentMode == modeDetector {
		log.Printf("switchMode(%v) blocked: detector mode active", m)

		return
	}

	switch currentMode {
	case modeFPV:
		stopFPV()
	case modeSpectrum:
		specAnalyzer.Stop()
	case modeREB:
		stopREB()
	}

	currentMode = m
	switch m {
	case modeFPV:
		caribou.OnVideo = palVideoFeed

		startFPV(activeFPVChan)
	case modeSpectrum:
		specAnalyzer.Start(specConfig)
	case modeREB:
		startREB()
	case modeDetector:
		startDetector()
	}

	log.Printf("mode → %v", m)
}

// finishCalibration transitions from modeCalibration back to modeSpectrum.
// Called by the calibration goroutine when a run completes (success or error).
func finishCalibration() {
	modeMu.Lock()
	defer modeMu.Unlock()

	if currentMode != modeCalibration {
		return
	}

	currentMode = modeSpectrum

	specAnalyzer.Start(specConfig)
	log.Printf("mode → spectrum (calibration complete)")
}

func (m appMode) String() string {
	switch m {
	case modeFPV:
		return "fpv"
	case modeCalibration:
		return "calibration"
	case modeREB:
		return "reb"
	case modeDetector:
		return "detector"
	default:
		return "spectrum"
	}
}

// /mode — GET returns current mode, POST ?m=spectrum|fpv switches

func modeHandler(w http.ResponseWriter, r *http.Request) {
	if m := r.URL.Query().Get("m"); m != "" {
		switch m {
		case "fpv":
			switchMode(modeFPV)
		case "spectrum":
			switchMode(modeSpectrum)
		case "reb":
			switchMode(modeREB)
		default:
			http.Error(w, `{"error":"unknown mode"}`, http.StatusBadRequest)

			return
		}
	}

	modeMu.Lock()
	cur := currentMode
	modeMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"mode":%q}`, cur.String())
}

// /spectrum/config — GET returns config, POST ?key=value updates it

func spectrumConfigHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	changed := false

	if v := q.Get("start"); v != "" {
		if hz, err := strconv.ParseInt(v, 10, 64); err == nil {
			specConfig.StartFreq = hz
			changed = true
		}
	}

	if v := q.Get("stop"); v != "" {
		if hz, err := strconv.ParseInt(v, 10, 64); err == nil {
			specConfig.StopFreq = hz
			changed = true
		}
	}

	if v := q.Get("gain"); v != "" {
		if g, err := strconv.Atoi(v); err == nil && g >= 0 && g <= 63 {
			specConfig.Gain = g
			changed = true
		}
	}

	if v := q.Get("agc"); v != "" {
		specConfig.AGC = v == "1" || v == "true"
		changed = true
	}

	if v := q.Get("samplerate"); v != "" {
		if sr, err := strconv.Atoi(v); err == nil {
			specConfig.SampleRate = sr
			changed = true
		}
	}

	if v := q.Get("fftsize"); v != "" {
		if fs, err := strconv.Atoi(v); err == nil {
			specConfig.FFTSize = fs
			changed = true
		}
	}

	if v := q.Get("radio"); v != "" {
		switch v {
		case "hif":
			specConfig.Radio = spectrumanalyzer.RadioHiF
		case "s1g":
			specConfig.Radio = spectrumanalyzer.RadioS1G
		}

		changed = true
	}

	// Restart sweep with new config if spectrum mode is active
	if changed {
		modeMu.Lock()
		active := currentMode == modeSpectrum
		modeMu.Unlock()

		if active {
			specAnalyzer.Start(specConfig) // Start() stops any running sweep first
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(specConfig)
}

// /spectrum/ws — WebSocket that pushes sweep results to the browser

func spectrumWSHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("spectrum ws upgrade: %v", err)

		return
	}

	ch := make(chan []spectrumanalyzer.PowerBin, 2)
	specAnalyzer.Subscribe(ch)

	defer func() {
		specAnalyzer.Unsubscribe(ch)
		conn.Close()
	}()

	// Send current config immediately so the UI can initialise
	if data, err := json.Marshal(map[string]any{
		"type":   "config",
		"config": specConfig,
	}); err == nil {
		conn.WriteMessage(websocket.TextMessage, data)
	}

	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	for bins := range ch {
		data, err := json.Marshal(map[string]any{
			"type": "sweep",
			"bins": bins,
		})
		if err != nil {
			continue
		}

		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			return
		}
	}
}
