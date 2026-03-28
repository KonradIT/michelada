package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/konradit/michelada/internal/caribou"
	"github.com/konradit/michelada/internal/pal"
)

// WebSocket hub — used by the spectrum analyzer (not started by default)

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	clients    = make(map[*websocket.Conn]bool)
	clientsMux sync.RWMutex
)

type RssiMessage struct {
	RSSI      float32 `json:"rssi"`
	Frequency float32 `json:"freq"`
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade: %v", err)

		return
	}

	clientsMux.Lock()
	clients[conn] = true
	clientsMux.Unlock()

	defer func() {
		clientsMux.Lock()
		delete(clients, conn)
		clientsMux.Unlock()
		conn.Close()
	}()

	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
}

// Spectrum analyzer

//go:embed assets/plotly.min.js
var plotlyJS string

//go:embed templates
var templateFS embed.FS

var tmpl = template.Must(template.ParseFS(templateFS, "templates/index.html"))

// FPV home page

func serveHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)

		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	labelsJSON, _ := json.Marshal(appCfg.Labels)

	err := tmpl.ExecuteTemplate(w, "index.html", map[string]template.JS{
		"Labels": template.JS(labelsJSON),
	})
	if err != nil {
		log.Printf("template: %v", err)
	}
}

// FPV status — returns whether a valid PAL frame was decoded recently

// fpvStatusHandler — GET /fpv/status — {"detected":true|false}
// "detected" is true only when the decoder has successfully emitted a JPEG
// frame within the last 3 seconds.  Pure RF noise or CW signals do not produce
// decodable frames, so this will not trigger on a signal generator.
func fpvStatusHandler(w http.ResponseWriter, r *http.Request) {
	detected := palDecoder != nil && palDecoder.RealVideoProbe(3*time.Second)

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"detected":%t}`, detected)
}

// fpvStandardHandler — GET /fpv/standard[?s=pal|ntsc] — gets or sets the video standard.
func fpvStandardHandler(w http.ResponseWriter, r *http.Request) {
	if v := r.URL.Query().Get("s"); v != "" {
		switch v {
		case "ntsc":
			palDecoder.SetStandard(pal.NTSC)
		case "pal":
			palDecoder.SetStandard(pal.PAL)
		default:
			http.Error(w, `{"error":"s must be pal or ntsc"}`, http.StatusBadRequest)

			return
		}
	}

	std := "pal"
	if palDecoder.GetStandard() == pal.NTSC {
		std = "ntsc"
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"standard":%q}`, std)
}

// SDR parameter control

// hifGain and hifBandwidth track current HiF settings so the UI can read them back.
var (
	hifGain      = 48
	hifBandwidth = 2_000_000 // default: widest IF filter (AT86RF215 max)
)

// sdrGainHandler — GET /sdr/gain?value=N — sets HiF RX gain (0-63).
func sdrGainHandler(w http.ResponseWriter, r *http.Request) {
	if v := r.URL.Query().Get("value"); v != "" {
		g, err := strconv.Atoi(v)
		if err != nil || g < 0 || g > 63 {
			http.Error(w, `{"error":"value must be 0-63"}`, http.StatusBadRequest)

			return
		}

		hifGain = g
		caribou.SetHiFGain(g)
		log.Printf("SDR: HiF gain → %d", g)
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"gain":%d}`, hifGain)
}

// sdrBandwidthHandler — GET /sdr/bandwidth?value=N — sets HiF IF filter BW (160000-2000000 Hz).
// AT86RF215 valid steps: 160k, 200k, 250k, 320k, 400k, 500k, 630k, 800k, 1M, 1.25M, 1.6M, 2M.
// SDK rounds to nearest step automatically.
func sdrBandwidthHandler(w http.ResponseWriter, r *http.Request) {
	if v := r.URL.Query().Get("value"); v != "" {
		bw, err := strconv.Atoi(v)
		if err != nil || bw < 160_000 || bw > 2_000_000 {
			http.Error(w, `{"error":"value must be 160000-2000000"}`, http.StatusBadRequest)

			return
		}

		hifBandwidth = bw
		caribou.SetHiFBandwidth(bw)
		log.Printf("SDR: HiF bandwidth → %d Hz", bw)
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"bandwidth":%d}`, hifBandwidth)
}

// main

var palDecoder *pal.PALDecoder

func main() {
	loadConfig()

	// Apply config defaults to spectrum analyzer
	if appCfg.DefaultSpectrumFreqs[0] > 0 && appCfg.DefaultSpectrumFreqs[1] > 0 {
		specConfig.StartFreq = int64(appCfg.DefaultSpectrumFreqs[0]) * 1_000_000
		specConfig.StopFreq = int64(appCfg.DefaultSpectrumFreqs[1]) * 1_000_000
	}

	palDecoder = pal.NewPALDecoder()
	decoder := palDecoder

	// Store the PAL decoder feed so spectrum.go's switchMode can restore it
	// when transitioning back to FPV mode.
	palVideoFeed = decoder.Feed

	http.HandleFunc("/", serveHome)
	http.HandleFunc("/plotly.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "max-age=86400")
		fmt.Fprint(w, plotlyJS)
	})
	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/video", decoder.ServeMJPEG)
	http.HandleFunc("/fpv/channels", serveFPVChannels)
	http.HandleFunc("/fpv/channel", setFPVChannelHandler)
	http.HandleFunc("/fpv/freq", setFPVFreqHandler)
	http.HandleFunc("/sdr/gain", sdrGainHandler)
	http.HandleFunc("/sdr/bandwidth", sdrBandwidthHandler)
	http.HandleFunc("/fpv/status", fpvStatusHandler)
	http.HandleFunc("/fpv/standard", fpvStandardHandler)
	http.HandleFunc("/mode", modeHandler)
	http.HandleFunc("/spectrum/config", spectrumConfigHandler)
	http.HandleFunc("/spectrum/ws", spectrumWSHandler)
	http.HandleFunc("/calibrate/start", startCalibrationHandler)
	http.HandleFunc("/calibrate/status", calStatusHandler)
	http.HandleFunc("/reb/ws", rebWSHandler)
	http.HandleFunc("/reb/rbw", rebRBWHandler)
	http.HandleFunc("/detector/status", detectorStatusHandler)
	http.HandleFunc("/detector/enable", detectorEnableHandler)
	http.HandleFunc("/detector/disable", detectorDisableHandler)

	go func() {
		log.Printf("Starting server on :8080")

		err := http.ListenAndServe(":8080", nil)
		if err != nil {
			log.Fatal(err)
		}
	}()

	caribou.Info()
	specAnalyzer.LoadCalibration() // load saved calibration if present

	if appCfg.DetectorStartOnBoot {
		log.Printf("Detector mode enabled — starting detector")

		currentMode = modeDetector

		startDetector()
	} else {
		// Default mode: spectrum analyzer
		specAnalyzer.Start(specConfig)
	}

	// Clean shutdown: call StopReceiving before the process exits so the
	// CaribouLite SDK can join its receive thread.  Without this, Ctrl+C
	// triggers a pthread_join deadlock inside the C++ destructor → SIGABRT.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down...")
	modeMu.Lock()
	m := currentMode
	modeMu.Unlock()

	switch m {
	case modeFPV:
		stopFPV()
	case modeREB:
		stopREB()
	case modeDetector:
		stopDetector()
	default:
		specAnalyzer.Stop()
	}

	log.Println("Done.")
}
