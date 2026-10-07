package api

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

type speedtestResult struct {
	WriteThroughputMBps float64 `json:"writeThroughputMBps"`
	ReadThroughputMBps  float64 `json:"readThroughputMBps"`
	Duration            string  `json:"duration"`
}

// speedtestBucket is the scratch directory a benchmark writes into. Its name
// is not a valid bucket name, so it can never collide with a real bucket.
const speedtestBucket = "__speedtest__"

// handleSpeedtest handles POST /api/v1/speedtest — runs a drive benchmark.
//
// One run at a time. Every run used to write the same key and remove the same
// directory, so two at once read each other's half-written file and one could
// delete the other's data mid-read, and each run also holds 64 MiB in memory.
// A second request while one runs is refused with 409.
func (h *APIHandler) handleSpeedtest(w http.ResponseWriter, r *http.Request) {
	if !h.speedtestRunning.CompareAndSwap(false, true) {
		writeError(w, http.StatusConflict, "a speedtest is already running, try again when it finishes")
		return
	}
	defer h.speedtestRunning.Store(false)

	const testSize = 64 * 1024 * 1024 // 64MB
	suffix, err := randomHex(8)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not name the benchmark object: "+err.Error())
		return
	}
	testKey := "__speedtest_object_" + suffix + "__"

	// Generate random data
	data := make([]byte, testSize)
	if _, err := rand.Read(data); err != nil {
		writeError(w, http.StatusInternalServerError, "could not generate benchmark data: "+err.Error())
		return
	}

	if err := h.engine.CreateBucketDir(speedtestBucket); err != nil {
		writeError(w, http.StatusInternalServerError, "could not create the benchmark directory: "+err.Error())
		return
	}
	// Errors from the cleanup used to be ignored, which left a 64 MiB file in
	// the data directory with nothing to say it was there.
	cleanup := func() error {
		if err := h.engine.DeleteObject(speedtestBucket, testKey); err != nil {
			return fmt.Errorf("remove the benchmark object: %w", err)
		}
		if err := h.engine.DeleteBucketDir(speedtestBucket); err != nil {
			return fmt.Errorf("remove the benchmark directory: %w", err)
		}
		return nil
	}

	// Write benchmark
	start := time.Now()
	_, _, err = h.engine.PutObject(speedtestBucket, testKey, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		if cerr := cleanup(); cerr != nil {
			slog.Warn("speedtest cleanup failed", "error", cerr)
		}
		writeError(w, http.StatusInternalServerError, "write benchmark failed: "+err.Error())
		return
	}
	writeDur := time.Since(start)

	// Read benchmark
	start = time.Now()
	reader, _, err := h.engine.GetObject(speedtestBucket, testKey)
	if err != nil {
		if cerr := cleanup(); cerr != nil {
			slog.Warn("speedtest cleanup failed", "error", cerr)
		}
		writeError(w, http.StatusInternalServerError, "read benchmark failed: "+err.Error())
		return
	}
	_, err = io.Copy(io.Discard, reader)
	reader.Close()
	readDur := time.Since(start)
	if err != nil {
		if cerr := cleanup(); cerr != nil {
			slog.Warn("speedtest cleanup failed", "error", cerr)
		}
		writeError(w, http.StatusInternalServerError, "read benchmark failed: "+err.Error())
		return
	}

	if err := cleanup(); err != nil {
		writeError(w, http.StatusInternalServerError, "the benchmark ran but could not "+err.Error())
		return
	}

	totalDur := writeDur + readDur

	writeJSON(w, http.StatusOK, speedtestResult{
		WriteThroughputMBps: float64(testSize) / (1024 * 1024) / writeDur.Seconds(),
		ReadThroughputMBps:  float64(testSize) / (1024 * 1024) / readDur.Seconds(),
		Duration:            totalDur.String(),
	})
}
