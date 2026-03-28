package main

import (
	"encoding/json"
	"log"
	"os"
	"os/user"
	"path/filepath"
)

// FreqLabel marks a frequency range on the spectrum analyzer chart.
type FreqLabel struct {
	Name  string `json:"name"`
	Start int    `json:"start"` // MHz
	Stop  int    `json:"stop"`  // MHz
}

// MicheladaConfig is the runtime configuration loaded from $HOME/michelada.json.
type MicheladaConfig struct {
	Scripts struct {
		OnVideoDetected []string `json:"on_video_detected"`
	} `json:"scripts"`
	DetectorStartOnBoot  bool        `json:"detector_start_on_boot"`
	DetectorBands        []string    `json:"detector_bands"`
	DetectorCooldown     int         `json:"detector_cooldown"`            // seconds
	DefaultSpectrumFreqs [2]int      `json:"default_spectrum_frequencies"` // [startMHz, stopMHz]
	Labels               []FreqLabel `json:"labels"`
}

var appCfg MicheladaConfig

func configPath() string {
	sudoUser := os.Getenv("SUDO_USER")
	if sudoUser != "" {
		if u, err := user.Lookup(sudoUser); err == nil {
			return filepath.Join(u.HomeDir, "michelada.json")
		}
	}

	home, _ := os.UserHomeDir()

	return filepath.Join(home, "michelada.json")
}

func loadConfig() {
	path := configPath()

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		log.Printf("[config] %s not found, using defaults", path)

		return
	}

	if err != nil {
		log.Printf("[config] read error: %v", err)

		return
	}

	if err := json.Unmarshal(data, &appCfg); err != nil {
		log.Printf("[config] parse error: %v", err)

		return
	}

	log.Printf("[config] loaded: detector_start_on_boot=%v bands=%v cooldown=%ds",
		appCfg.DetectorStartOnBoot, appCfg.DetectorBands, appCfg.DetectorCooldown)
}

// saveConfig writes the current appCfg back to $HOME/michelada.json.
func saveConfig() error {
	data, err := json.MarshalIndent(&appCfg, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(configPath(), data, 0o644)
}
