// Package upload provides a streaming replacement for the
// `data, err := io.ReadAll(file)` pattern used in the existing /analyze
// handler. Instead of buffering the entire upload in RAM, it copies
// directly from the multipart file part to a temporary file on disk while
// computing a SHA-256 hash of the original bytes in the same pass — the
// hash the rest of the pipeline (order metadata, Google Drive dedup,
// analysis caching) needs anyway per the project's storage requirements.
package upload

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// SavedFile describes an upload that has been streamed to disk.
type SavedFile struct {
	Path       string // temp file path; caller owns cleanup (defer Remove)
	Size       int64
	SHA256Hex  string
	OriginalFn string
}

// Remove deletes the temporary file. Safe to call multiple times.
func (s SavedFile) Remove() {
	if s.Path != "" {
		_ = os.Remove(s.Path)
	}
}

// StreamToTemp copies src (already wrapped by the caller in
// http.MaxBytesReader / io.LimitReader as appropriate) into a new temp file
// under dir (os.TempDir() if dir == ""), hashing as it goes.
//
// It never calls io.ReadAll and never holds the whole file in memory: the
// only buffer involved is io.Copy's internal 32KiB chunk buffer.
func StreamToTemp(dir string, originalFilename string, src io.Reader) (SavedFile, error) {
	f, err := os.CreateTemp(dir, "stl-upload-*.bin")
	if err != nil {
		return SavedFile{}, fmt.Errorf("create temp file: %w", err)
	}
	// Ensure we never leak the fd or a partial file on any error path below.
	success := false
	defer func() {
		_ = f.Close()
		if !success {
			_ = os.Remove(f.Name())
		}
	}()

	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hasher), src)
	if err != nil {
		return SavedFile{}, fmt.Errorf("write upload to temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return SavedFile{}, fmt.Errorf("sync temp file: %w", err)
	}

	success = true
	return SavedFile{
		Path:       f.Name(),
		Size:       n,
		SHA256Hex:  hex.EncodeToString(hasher.Sum(nil)),
		OriginalFn: originalFilename,
	}, nil
}
