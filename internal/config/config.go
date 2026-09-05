// Package config centralizes the environment-variable knobs described in
// the STL engine upgrade: upload size limit, concurrency limit, and
// per-request analysis timeout. All have safe production defaults, so the
// service runs correctly with zero configuration.
package config

import (
	"os"
	"strconv"
	"time"
)

// Config holds the tunables read once at startup.
type Config struct {
	// MaxUploadSizeMB caps the size of an incoming STL/model upload.
	// Env: MAX_UPLOAD_SIZE_MB (default 50, matching the existing limit).
	MaxUploadSizeMB int64

	// MaxConcurrentAnalysis bounds how many /analyze requests run their
	// (CPU-bound) geometry pass at the same time, so one huge upload can't
	// starve every other customer's request.
	// Env: MAX_CONCURRENT_ANALYSIS (default 4).
	MaxConcurrentAnalysis int

	// AnalysisTimeout bounds how long a single analysis is allowed to run
	// before it is cancelled via context.
	// Env: ANALYSIS_TIMEOUT_SECONDS (default 30).
	AnalysisTimeout time.Duration

	// MaxTriangles is a hard safety cap independent of file size, guarding
	// against a small file that decompresses/expands into an absurd
	// triangle count. Env: MAX_TRIANGLES (default 20,000,000).
	MaxTriangles uint64

	// TopologyBudget is the triangle-count ceiling for manifold/component
	// analysis. Above it, volume/dimensions are still computed exactly,
	// but watertightness is not verified. Env: TOPOLOGY_BUDGET_TRIANGLES
	// (default 6,000,000).
	TopologyBudget uint64
}

// Load reads configuration from the environment, applying defaults for
// anything unset or invalid. It never panics and never requires any
// variable to be present.
func Load() Config {
	return Config{
		MaxUploadSizeMB:       envInt64("MAX_UPLOAD_SIZE_MB", 50),
		MaxConcurrentAnalysis: envInt("MAX_CONCURRENT_ANALYSIS", 4),
		AnalysisTimeout:       time.Duration(envInt64("ANALYSIS_TIMEOUT_SECONDS", 30)) * time.Second,
		MaxTriangles:          envUint64("MAX_TRIANGLES", 20_000_000),
		TopologyBudget:        envUint64("TOPOLOGY_BUDGET_TRIANGLES", 6_000_000),
	}
}

// MaxUploadSizeBytes is a convenience accessor for http.MaxBytesReader /
// ParseMultipartForm call sites.
func (c Config) MaxUploadSizeBytes() int64 {
	return c.MaxUploadSizeMB << 20
}

func envInt64(name string, def int64) int64 {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

func envInt(name string, def int) int {
	return int(envInt64(name, int64(def)))
}

func envUint64(name string, def uint64) uint64 {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || v == 0 {
		return def
	}
	return v
}
