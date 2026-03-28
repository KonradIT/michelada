package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/konradit/michelada/internal/caribou"
)

// REB (РЕБ) - радіоелектро́нна боротьба́ checks for efficiency of working
// jammers and other EW devices.
// Fast multi-band HiF scan.  Minimal settle/measure windows + per-band
// broadcast so each quadrant updates as soon as its band is done.
type rebBandDef struct {
	Label string
	Freqs []int64
}

type rebCell struct {
	Freq  float64 `json:"freq"`
	Power float32 `json:"power"`
}

type rebBandResult struct {
	Label string    `json:"label"`
	Cells []rebCell `json:"cells"`
}

// 24 frequencies total — one full cycle ≈ 10 ms at 4 Msps.
var rebBands = []rebBandDef{
	{
		Label: "750-915 MHz",
		Freqs: []int64{
			750_000_000, 800_000_000, 850_000_000, 868_000_000,
			885_000_000, 900_000_000, 910_000_000, 915_000_000,
		},
	},
	{
		Label: "2.4 GHz",
		Freqs: []int64{
			2_400_000_000, 2_420_000_000, 2_450_000_000,
			2_470_000_000, 2_500_000_000,
		},
	},
	{
		Label: "5.2 GHz",
		Freqs: []int64{
			5_150_000_000, 5_200_000_000, 5_250_000_000,
			5_300_000_000, 5_350_000_000,
		},
	},
	{
		Label: "5.8 GHz",
		Freqs: []int64{
			5_740_000_000, 5_770_000_000, 5_800_000_000,
			5_820_000_000, 5_840_000_000, 5_860_000_000,
		},
	},
}

var (
	rebClients   = make(map[*websocket.Conn]bool)
	rebClientsMu sync.RWMutex
	rebStopCh    chan struct{}
	rebMeasure   int32 = rebMeasureSamplesDefault // atomic-ish; read by sweep goroutine
	rebMeasureMu sync.Mutex
)

func rebRBWHandler(w http.ResponseWriter, r *http.Request) {
	if v := r.URL.Query().Get("v"); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil && n >= 256 && n <= 4096 {
			rebMeasureMu.Lock()
			rebMeasure = int32(n)
			rebMeasureMu.Unlock()
			log.Printf("[REB] measure samples → %d", n)
		}
	}

	rebMeasureMu.Lock()
	cur := rebMeasure
	rebMeasureMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"measure_samples":%d}`, cur)
}

func rebWSHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[REB] ws upgrade: %v", err)

		return
	}

	rebClientsMu.Lock()
	rebClients[conn] = true
	rebClientsMu.Unlock()

	defer func() {
		rebClientsMu.Lock()
		delete(rebClients, conn)
		rebClientsMu.Unlock()
		conn.Close()
	}()

	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

func rebBroadcast(data []byte) {
	rebClientsMu.Lock()
	defer rebClientsMu.Unlock()

	for c := range rebClients {
		err := c.WriteMessage(websocket.TextMessage, data)
		if err != nil {
			c.Close()
			delete(rebClients, c)
		}
	}
}

func startREB() {
	rebStopCh = make(chan struct{})
	go rebSweepLoop(rebStopCh)
}

func stopREB() {
	if rebStopCh != nil {
		close(rebStopCh)
		rebStopCh = nil
	}

	time.Sleep(30 * time.Millisecond)
}

// Aggressive sample windows — PLL relock is ~100 µs, these are enough.
const (
	rebSettleSamples         = 512  // 0.13 ms at 4 Msps
	rebMeasureSamplesDefault = 1024 // 0.26 ms at 4 Msps
)

func rebSweepLoop(stop chan struct{}) {
	var (
		mu      sync.Mutex
		pending []float32
		notify  = make(chan struct{}, 1)
	)

	caribou.OnVideo = func(samples []float32) {
		mu.Lock()

		pending = append(pending, samples...)
		mu.Unlock()

		select {
		case notify <- struct{}{}:
		default:
		}
	}

	caribou.SetHiFSampleRate(4_000_000)
	caribou.SetHiFGain(48)
	caribou.SetHiFAGC(false)
	caribou.SetHiFBandwidth(2_000_000)

	drain := func() {
		mu.Lock()
		pending = pending[:0]
		mu.Unlock()
	}
	waitN := func(n int) bool {
		for {
			mu.Lock()
			have := len(pending)
			mu.Unlock()

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
		for bi, band := range rebBands {
			var cells []rebCell

			for fi, freq := range band.Freqs {
				select {
				case <-stop:
					caribou.StopHiF()

					caribou.OnVideo = nil

					return
				default:
				}

				if fi == 0 {
					// Full Stop→Start at each band boundary to recover
					// from any RFFC5072 PLL deactivation (same pattern
					// as setFPVChannel in fpv.go).
					caribou.StopHiF()
					caribou.StartHiF(int(freq))
				} else {
					caribou.SetHiFFreq(int(freq))
				}

				// Single drain→settle→measure pass (no double drain)
				rebMeasureMu.Lock()
				mSamples := int(rebMeasure)
				rebMeasureMu.Unlock()

				drain()

				if !waitN(rebSettleSamples + mSamples) {
					caribou.StopHiF()

					caribou.OnVideo = nil

					return
				}
				// Skip settle portion, take only the measure tail
				mu.Lock()

				n := min(len(pending), mSamples)

				start := len(pending) - n

				var sum float32
				for _, v := range pending[start : start+n] {
					sum += v
				}

				pending = pending[:0]
				mu.Unlock()

				mean := float64(sum) / float64(n)
				if mean < 1e-12 {
					mean = 1e-12
				}

				powerDB := float32(20 * math.Log10(mean))
				powerDB = specAnalyzer.ApplyCalibration(freq, powerDB)
				cells = append(cells, rebCell{Freq: float64(freq), Power: powerDB})
			}

			// Broadcast this quadrant immediately — don't wait for all 4
			data, _ := json.Marshal(map[string]any{
				"type":  "reb_band",
				"index": bi,
				"band":  rebBandResult{Label: band.Label, Cells: cells},
			})
			rebBroadcast(data)
		}

		log.Printf("[REB] cycle complete")
	}
}
