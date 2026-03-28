package pal

// Batch video decoder for CaribouLite HiF at 4 Msps using IQ power-envelope
// slope detection. Supports PAL (625 lines, 50 Hz) and NTSC (525 lines, 59.94 Hz).
//
// Algorithm (adapted from sdr-experiments/fpv_pal.py):
//   1. Accumulate two full frames of samples
//   2. Compute adaptive sync/black threshold from first ~50 lines
//   3. Find H-sync rising edges with minimum-spacing filter
//   4. Detect V-sync by inter-line gap analysis
//   5. Extract active-video samples per line (skip back porch)
//   6. Normalise with per-frame 1st-99th percentile of all extracted lines
//   7. Resample each line to outWidth and emit a JPEG

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"math"
	"net/http"
	"slices"
	"sync"
	"time"
)

// Standard selects the analog video standard decoded.
type Standard int

const (
	PAL  Standard = iota // 625 lines, 50 Hz
	NTSC                 // 525 lines, 59.94 Hz
)

// Color subcarrier detection constants.
const (
	palSubcarrierHz  = 4_433_618.75 // PAL  Fsc — QAM-modulated U/V chroma
	ntscSubcarrierHz = 3_579_545.0  // NTSC Fsc
	sdrSampleRate    = 4_000_000.0  // CaribouLite HiF sample rate
	colorProbeN      = 4096         // Goertzel analysis window (~1 ms at 4 Msps)
	colorSNRMin      = 6.0          // subcarrier must be ≥ ~8 dB above local noise
)

// standardConf holds all timing parameters for one video standard.
type standardConf struct {
	name             string
	samplesPerLine   int
	backPorchSamples int
	activeSamples    int
	vBlankLines      int
	activeLines      int
	outLines         int
	outWidth         int
	batchSamples     int
	wideThreshold    int // V-sync: samples-below-threshold per inter-line gap
	minFieldSpacing  int // V-sync: minimum line-index gap between field detections
}

var palConf = standardConf{
	name:             "PAL",
	samplesPerLine:   256, // 4 MHz / 15 625 Hz
	backPorchSamples: 8,
	activeSamples:    205,
	vBlankLines:      25,
	activeLines:      287, // 312 − 25
	outLines:         574, // 287 × 2 fields
	outWidth:         720,
	batchSamples:     2 * 625 * 256, // 320 000
	wideThreshold:    102,           // 256 × 40 %
	minFieldSpacing:  229,           // 287 × 80 %
}

var ntscConf = standardConf{
	name:             "NTSC",
	samplesPerLine:   254, // 4 MHz / 15 734 Hz ≈ 254
	backPorchSamples: 6,   // slightly shorter back porch than PAL
	activeSamples:    200,
	vBlankLines:      21,
	activeLines:      240, // 262 − 21 − 1
	outLines:         480, // 240 × 2 fields
	outWidth:         720,
	batchSamples:     2 * 525 * 254, // 266 700
	wideThreshold:    101,           // 254 × 40 %
	minFieldSpacing:  192,           // 240 × 80 %
}

type batchMsg struct {
	samples []float32
	conf    standardConf
}

// PALDecoder accumulates IQ power-envelope samples, processes them in
// frame-sized batches, and pushes JPEG frames to registered MJPEG clients.
// Despite the name it supports both PAL and NTSC; default is PAL.
type PALDecoder struct {
	mu      sync.Mutex
	conf    standardConf
	pending []float32

	batches chan batchMsg

	clientsMu sync.RWMutex
	clients   map[chan []byte]bool

	// diagnostics
	dbgBatches  int64
	dbgFramesOK int64
	dbgSkip     int64
	dbgLastLog  time.Time

	lastFrameAt  time.Time // wall time of the most recently emitted JPEG frame
	probeSamples []float32 // recent batch window for color subcarrier probe
}

func NewPALDecoder() *PALDecoder {
	d := &PALDecoder{
		conf:    palConf,
		pending: make([]float32, 0, palConf.batchSamples*2),
		batches: make(chan batchMsg, 4),
		clients: make(map[chan []byte]bool),
	}
	go d.processor()

	return d
}

// SetStandard switches between PAL and NTSC. Any accumulated samples are discarded.
func (d *PALDecoder) SetStandard(s Standard) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if s == NTSC {
		d.conf = ntscConf
	} else {
		d.conf = palConf
	}

	d.pending = d.pending[:0]
	log.Printf("[VIDEO] standard → %s", d.conf.name)
}

// GetStandard returns the current Standard.
func (d *PALDecoder) GetStandard() Standard {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conf.name == "NTSC" {
		return NTSC
	}

	return PAL
}

// Feed queues IQ power-envelope samples for batch processing.
// Safe to call from any goroutine (including the CGo callback thread).
func (d *PALDecoder) Feed(samples []float32) {
	d.mu.Lock()
	d.pending = append(d.pending, samples...)

	conf := d.conf
	for len(d.pending) >= conf.batchSamples {
		batch := make([]float32, conf.batchSamples)
		copy(batch, d.pending[:conf.batchSamples])

		d.pending = d.pending[conf.batchSamples:]
		select {
		case d.batches <- batchMsg{samples: batch, conf: conf}:
		default:
		}
	}
	d.mu.Unlock()
}

func (d *PALDecoder) processor() {
	for msg := range d.batches {
		d.processBatch(msg.samples, msg.conf)
	}
}

func (d *PALDecoder) processBatch(batch []float32, c standardConf) {
	d.mu.Lock()
	d.dbgBatches++
	d.mu.Unlock()

	// ── 1. Adaptive threshold from first ~50 lines ────────────────────────
	winN := 50 * c.samplesPerLine
	win := make([]float32, winN)
	copy(win, batch[:winN])
	slices.Sort(win)

	syncLevel := mean32(win[:winN*5/100])

	blackLevel := mean32(win[winN*8/100 : winN*15/100])
	if blackLevel <= syncLevel {
		d.mu.Lock()
		d.dbgSkip++
		d.mu.Unlock()
		log.Printf("[%s] batch skipped: no sync/black separation (sync=%g black=%g)", c.name, syncLevel, blackLevel)

		return
	}

	threshold := (syncLevel + blackLevel) / 2.0

	// ── 2. H-sync detection: rising edges with minimum-spacing filter ─────
	minSpacing := c.samplesPerLine * 4 / 5

	var (
		lineStarts []int
		rawEdges   int
	)

	inSync := batch[0] < threshold
	for i := 1; i < len(batch); i++ {
		nowSync := batch[i] < threshold
		if inSync && !nowSync {
			rawEdges++

			if len(lineStarts) == 0 || i-lineStarts[len(lineStarts)-1] >= minSpacing {
				lineStarts = append(lineStarts, i)
			}
		}

		inSync = nowSync
	}

	var spacingMean, spacingMin, spacingMax int
	if len(lineStarts) >= 2 {
		spacingMin = lineStarts[1] - lineStarts[0]
		spacingMax = spacingMin

		var spacingSum int

		for i := 1; i < len(lineStarts); i++ {
			sp := lineStarts[i] - lineStarts[i-1]

			spacingSum += sp
			if sp < spacingMin {
				spacingMin = sp
			}

			if sp > spacingMax {
				spacingMax = sp
			}
		}

		spacingMean = spacingSum / (len(lineStarts) - 1)
	}

	log.Printf("[%s] hsync: raw=%d lines=%d spacing mean=%d min=%d max=%d thr=%g sync=%g black=%g",
		c.name, rawEdges, len(lineStarts), spacingMean, spacingMin, spacingMax, threshold, syncLevel, blackLevel)

	// ── 3. V-sync detection ───────────────────────────────────────────────
	fieldStarts := detectVSync(batch, lineStarts, threshold, c)
	log.Printf("[%s] vsync: fields=%d starts=%v", c.name, len(fieldStarts), fieldStarts)

	// ── 4. Frame alignment ────────────────────────────────────────────────
	startIdx := c.vBlankLines
	usedFieldStart := -1

	for _, fs := range fieldStarts {
		candidate := fs + c.vBlankLines
		if candidate+c.outLines <= len(lineStarts) {
			startIdx = candidate
			usedFieldStart = fs

			break
		}
	}

	if startIdx+c.outLines > len(lineStarts) {
		d.mu.Lock()
		d.dbgSkip++
		d.mu.Unlock()
		log.Printf("[%s] batch skipped: need %d lines from idx %d, have %d (fieldStarts=%v)",
			c.name, c.outLines, startIdx, len(lineStarts), fieldStarts)

		return
	}

	log.Printf("[%s] frame align: startIdx=%d usedFieldStart=%d", c.name, startIdx, usedFieldStart)

	// ── 5. Extract active video (2 fields) ───────────────────────────────
	lines := make([][]float32, c.outLines)
	for i := range c.outLines {
		aStart := lineStarts[startIdx+i] + c.backPorchSamples

		aEnd := aStart + c.activeSamples
		if aEnd > len(batch) {
			d.mu.Lock()
			d.dbgSkip++
			d.mu.Unlock()

			return
		}

		line := make([]float32, c.activeSamples)
		copy(line, batch[aStart:aEnd])
		lines[i] = line
	}

	// ── 5b. FPV presence check: real feed has ≥20 consecutive dark lines ──
	const fpvBlackRunThreshold = 20

	hasFPVPattern := false
	blackRun := 0

	for _, line := range lines {
		if mean32(line) < threshold {
			blackRun++
			if blackRun >= fpvBlackRunThreshold {
				hasFPVPattern = true

				break
			}
		} else {
			blackRun = 0
		}
	}

	log.Printf("[%s] fpv pattern: hasFPVPattern=%v blackRun=%d", c.name, hasFPVPattern, blackRun)

	// ── 6. Per-frame percentile normalisation ─────────────────────────────
	all := make([]float32, c.outLines*c.activeSamples)
	for y, line := range lines {
		copy(all[y*c.activeSamples:], line)
	}

	slices.Sort(all)
	n := len(all)
	normLo := all[n*1/100]

	normHi := all[n*99/100]
	if normHi-normLo < 1e-12 {
		d.mu.Lock()
		d.dbgSkip++
		d.mu.Unlock()

		return
	}

	// ── 7. Build grayscale image, resampling each line to outWidth ────────
	img := image.NewGray(image.Rect(0, 0, c.outWidth, c.outLines))
	for y, line := range lines {
		for x := range c.outWidth {
			t := float32(x) / float32(c.outWidth-1) * float32(c.activeSamples-1)
			lo := int(t)

			hi := lo + 1
			if hi >= c.activeSamples {
				hi = c.activeSamples - 1
			}

			frac := t - float32(lo)
			s := line[lo]*(1-frac) + line[hi]*frac

			norm := (s - normLo) / (normHi - normLo)
			if norm < 0 {
				norm = 0
			} else if norm > 1 {
				norm = 1
			}

			img.SetGray(x, y, color.Gray{Y: uint8(norm * 255)})
		}
	}

	// ── 8. JPEG encode ─────────────────────────────────────────────────────
	var buf bytes.Buffer

	err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 75})
	if err != nil {
		d.mu.Lock()
		d.dbgSkip++
		d.mu.Unlock()

		return
	}

	d.mu.Lock()
	d.dbgFramesOK++

	ok := d.dbgFramesOK
	if hasFPVPattern {
		d.lastFrameAt = time.Now()
		// Stash a window of raw samples for StreamContainsColorSubcarrier
		if len(batch) >= colorProbeN {
			mid := (len(batch) - colorProbeN) / 2
			probe := make([]float32, colorProbeN)
			copy(probe, batch[mid:mid+colorProbeN])
			d.probeSamples = probe
		}
	}
	d.mu.Unlock()

	log.Printf("[%s] frame ok: %dx%d  field=%d  lines=%d  norm=[%g,%g]  total=%d",
		c.name, c.outWidth, c.outLines, usedFieldStart, len(lineStarts), normLo, normHi, ok)

	frame := buf.Bytes()

	d.clientsMu.RLock()

	for ch := range d.clients {
		select {
		case ch <- frame:
		default:
		}
	}

	d.clientsMu.RUnlock()

	if now := time.Now(); now.Sub(d.dbgLastLog) >= 5*time.Second {
		d.mu.Lock()
		log.Printf("[%s] status: batches=%d  frames=%d  skip=%d",
			c.name, d.dbgBatches, d.dbgFramesOK, d.dbgSkip)
		d.dbgLastLog = now
		d.mu.Unlock()
	}
}

// detectVSync finds field boundaries using inter-line gap analysis.
// For each pair of consecutive H-sync edges, count samples below threshold;
// if count > wideThreshold the gap contains a broad V-sync pulse.
func detectVSync(batch []float32, lineStarts []int, threshold float32, c standardConf) []int {
	var fieldStarts []int

	for i := 1; i < len(lineStarts); i++ {
		segStart := lineStarts[i-1]

		segEnd := min(lineStarts[i], len(batch))

		lowCount := 0

		for j := segStart; j < segEnd; j++ {
			if batch[j] < threshold {
				lowCount++
			}
		}

		if lowCount > c.wideThreshold {
			if len(fieldStarts) == 0 || i-fieldStarts[len(fieldStarts)-1] > c.minFieldSpacing {
				fieldStarts = append(fieldStarts, i)
			}
		}
	}

	return fieldStarts
}

// RealVideoProbe reports whether a valid frame was successfully emitted
// within the given window. The color subcarrier check is logged for
// diagnostics but not gated on — the AT86RF215's 2 MHz IF bandwidth
// filters out the 4.43 MHz PAL / 3.58 MHz NTSC chroma subcarrier before
// it reaches the sample stream, so it cannot be detected from
// power-envelope data at the current hardware configuration.
func (d *PALDecoder) RealVideoProbe(window time.Duration) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.lastFrameAt.IsZero() || time.Since(d.lastFrameAt) >= window {
		return false
	}
	// Log color subcarrier analysis for diagnostics (not a gate)
	if len(d.probeSamples) > 0 {
		std := PAL
		if d.conf.name == "NTSC" {
			std = NTSC
		}

		StreamContainsColorSubcarrier(d.probeSamples, std)
	}

	return true
}

// StreamContainsColorSubcarrier checks for the color-information signal
// subcarrier at 4.43361875 MHz (PAL) or 3.579545 MHz (NTSC). Chrominance
// is modulated onto luma via QAM — U and V chroma-difference channels are
// each amplitude-modulated onto the subcarrier at 90° phase offset.
//
// At 4 Msps the subcarrier aliases into the sampled stream (PAL → ~433.6 kHz,
// NTSC → ~420.5 kHz). A Goertzel filter measures energy at the aliased
// frequency and compares it to nearby off-frequencies as a local noise
// reference. If the subcarrier stands ≥ ~8 dB above the noise floor the
// signal contains color.
func StreamContainsColorSubcarrier(samples []float32, std Standard) bool {
	if len(samples) < colorProbeN {
		log.Printf("[color] too few samples: %d < %d", len(samples), colorProbeN)

		return false
	}

	// Select subcarrier for the active standard
	fsc := palSubcarrierHz
	stdName := "PAL"

	if std == NTSC {
		fsc = ntscSubcarrierHz
		stdName = "NTSC"
	}

	// Compute aliased frequency within [0, fs/2]
	aliased := math.Mod(fsc, sdrSampleRate)
	if aliased > sdrSampleRate/2 {
		aliased = sdrSampleRate - aliased
	}

	log.Printf("[color] %s fsc=%.2f Hz aliased=%.2f Hz", stdName, fsc, aliased)

	// Use a centered window of colorProbeN samples
	off := (len(samples) - colorProbeN) / 2
	window := samples[off : off+colorProbeN]

	// Sample stats for sanity check
	var minV, maxV, sum float32

	minV = window[0]

	maxV = window[0]
	for _, v := range window {
		sum += v
		if v < minV {
			minV = v
		}

		if v > maxV {
			maxV = v
		}
	}

	mean := sum / float32(len(window))
	log.Printf("[color] window: N=%d min=%.6g max=%.6g mean=%.6g", len(window), minV, maxV, mean)

	// Goertzel at the subcarrier
	scPower := goertzelPower(window, aliased, sdrSampleRate)

	// Goertzel at ±30 kHz / ±50 kHz for local noise reference
	var noisePower float64

	offsets := [4]float64{-50_000, -30_000, 30_000, 50_000}
	for _, d := range offsets {
		f := aliased + d
		if f < 0 {
			f = -f
		}

		noisePower += goertzelPower(window, f, sdrSampleRate)
	}

	noisePower /= float64(len(offsets))

	var snr float64
	if noisePower < 1e-20 {
		snr = -1 // indicates noise floor is effectively zero

		log.Printf("[color] scPower=%.6g noisePower=%.6g (floor) snr=N/A → %v",
			scPower, noisePower, scPower > 1e-10)

		return scPower > 1e-10
	}

	snr = scPower / noisePower
	pass := snr > colorSNRMin
	log.Printf("[color] scPower=%.6g noisePower=%.6g snr=%.2f threshold=%.1f → %v",
		scPower, noisePower, snr, colorSNRMin, pass)

	return pass
}

// goertzelPower returns the squared magnitude of the DFT at targetFreq Hz
// for the given sample buffer at sampleRate Hz. O(N) — no full FFT needed.
func goertzelPower(samples []float32, targetFreq, sampleRate float64) float64 {
	n := len(samples)
	k := targetFreq / sampleRate * float64(n)
	w := 2.0 * math.Pi * k / float64(n)
	coeff := 2.0 * math.Cos(w)

	var s1, s2 float64
	for _, x := range samples {
		s0 := float64(x) + coeff*s1 - s2
		s2 = s1
		s1 = s0
	}

	return s1*s1 + s2*s2 - coeff*s1*s2
}

func mean32(s []float32) float32 {
	if len(s) == 0 {
		return 0
	}

	var sum float32
	for _, v := range s {
		sum += v
	}

	return sum / float32(len(s))
}

// ── MJPEG plumbing ────────────────────────────────────────────────────────────

func (d *PALDecoder) addClient(ch chan []byte) {
	d.clientsMu.Lock()
	d.clients[ch] = true
	d.clientsMu.Unlock()
}

func (d *PALDecoder) removeClient(ch chan []byte) {
	d.clientsMu.Lock()
	delete(d.clients, ch)
	d.clientsMu.Unlock()
}

func (d *PALDecoder) noSignalFrame() []byte {
	d.mu.Lock()
	c := d.conf
	d.mu.Unlock()

	img := image.NewGray(image.Rect(0, 0, c.outWidth, c.outLines))
	for i := range img.Pix {
		img.Pix[i] = 20
	}

	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 50}) //nolint:errcheck

	return buf.Bytes()
}

func writeMJPEGFrame(w http.ResponseWriter, frame []byte) error {
	if _, err := fmt.Fprintf(w,
		"--jpgboundary\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n",
		len(frame)); err != nil {
		return err
	}

	if _, err := w.Write(frame); err != nil {
		return err
	}

	_, err := fmt.Fprintf(w, "\r\n")

	return err
}

// ServeMJPEG handles GET /video — streams MJPEG to the client.
func (d *PALDecoder) ServeMJPEG(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=jpgboundary")
	w.Header().Set("Cache-Control", "no-cache")

	flusher, canFlush := w.(http.Flusher)

	err := writeMJPEGFrame(w, d.noSignalFrame())
	if err != nil {
		return
	}

	if canFlush {
		flusher.Flush()
	}

	ch := make(chan []byte, 3)
	d.addClient(ch)

	defer func() {
		d.removeClient(ch)
		close(ch)
	}()

	for {
		select {
		case frame, ok := <-ch:
			if !ok {
				return
			}

			err := writeMJPEGFrame(w, frame)
			if err != nil {
				return
			}

			if canFlush {
				flusher.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}
