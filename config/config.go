package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

// Config holds non-secret runtime configuration. Every field is overridable
// through an environment variable so the binary stays portable.
type Config struct {
	Addr string
	// DBPath is the SQLite file location.
	DBPath string
	// Tick is the wall-clock re-check interval. The scheduler ticker is the
	// authority on what has expired; the heap only orders the work.
	Tick time.Duration
	// ConfirmThreshold is the confidence at or above which a parse may be
	// scheduled without a confirmation preview.
	ConfirmThreshold float64
	// SoundOn enables the host audio alert.
	SoundOn bool
	// ToastOn enables the host desktop toast.
	ToastOn bool
}

// Dir returns the platform config directory used for the database and VAPID keys.
func Dir() string {
	if d := os.Getenv("LOCAL_ASSISTANT_HOME"); d != "" {
		return d
	}
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("APPDATA"); d != "" {
			return filepath.Join(d, "local-assistant")
		}
	case "darwin":
		return filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "local-assistant")
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "local-assistant")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".config", "local-assistant")
	}
	return ".local-assistant"
}

func Load() Config {
	dir := Dir()
	return Config{
		Addr:             env("LOCAL_ASSISTANT_ADDR", "127.0.0.1:8080"),
		DBPath:           env("LOCAL_ASSISTANT_DB", filepath.Join(dir, "localassistant.db")),
		Tick:             time.Duration(envInt("LOCAL_ASSISTANT_TICK_SECONDS", 15)) * time.Second,
		ConfirmThreshold: envFloat("LOCAL_ASSISTANT_CONFIRM_THRESHOLD", 0.85),
		SoundOn:          envBool("LOCAL_ASSISTANT_SOUND", true),
		ToastOn:          envBool("LOCAL_ASSISTANT_TOAST", true),
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}
