package stlengine

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
)

const binaryHeaderSize = 84
const binaryTriangleRecordSize = 50

// detectFormat classifies an STL file from its header bytes and total size.
//
// It deliberately does NOT decide "binary vs ASCII" by checking whether the
// file starts with the literal text "solid" — a binary STL's 80-byte header
// is free-form and frequently contains that word (many CAD tools write
// "solid ExportedFromXYZ..." into the binary header). Instead it verifies
// the binary size formula (84 + count*50) using overflow-safe uint64 math,
// and only falls back to the ASCII heuristic when that formula does not
// match.
//
// peek must contain at least the first min(size, len(peek)) bytes of the
// file, starting at offset 0. size is the total file size in bytes.
func detectFormat(peek []byte, size int64) (format string, triangleCount uint32, err error) {
	if size <= 0 {
		return "", 0, fmt.Errorf("%w: file is empty", errInvalid(ErrEmptyModel))
	}

	trimmed := bytes.TrimLeft(peek, " \t\r\n")

	// --- Binary check (authoritative when it matches) ---
	if size >= binaryHeaderSize && len(peek) >= binaryHeaderSize {
		count := binary.LittleEndian.Uint32(peek[80:84])
		// uint64 math: count is at most 2^32-1, *50 is at most ~2.1e11,
		// nowhere near overflowing uint64. Safe by construction.
		expected := uint64(binaryHeaderSize) + uint64(count)*uint64(binaryTriangleRecordSize)

		switch {
		case expected == uint64(size):
			return "binary", count, nil
		case uint64(size) > expected && count > 0:
			// More bytes than the header promises: extra trailing data
			// (some tools pad files, or a checksum/footer was appended).
			// The first `count` triangles are still well-defined, so we
			// accept the file and simply ignore the trailing bytes.
			return "binary", count, nil
		case uint64(size) < expected:
			// Looks like a binary STL, but truncated: fewer bytes than the
			// declared triangle count requires. Do not guess — a partial
			// read would silently under-report volume, which is exactly
			// the "fake result" the customer-facing pricing must never do.
			if !looksLikeASCII(trimmed) {
				return "", 0, fmt.Errorf("%w: binary STL declares %d triangles (needs %d bytes) but file is only %d bytes",
					errInvalid(ErrTruncatedSTL), count, expected, size)
			}
		}
	}

	// --- ASCII check ---
	if looksLikeASCII(trimmed) {
		return "ascii", 0, nil
	}

	// Small files that are too short to contain even a binary header AND
	// don't look like ASCII text.
	if size < binaryHeaderSize {
		return "", 0, fmt.Errorf("%w: file is too small to be a valid STL (%d bytes)", errInvalid(ErrInvalidFile), size)
	}

	return "", 0, fmt.Errorf("%w: file does not match binary or ASCII STL structure", errInvalid(ErrInvalidFile))
}

// looksLikeASCII applies a bounded, case-insensitive heuristic: the file
// must start with "solid" and, within the peeked window, contain both
// "facet" and "vertex". This mirrors (and hardens) common STL-sniffing
// logic without ever loading the full file to check it.
func looksLikeASCII(trimmed []byte) bool {
	if len(trimmed) < 5 {
		return false
	}
	windowLen := len(trimmed)
	if windowLen > 8192 {
		windowLen = 8192
	}
	lower := strings.ToLower(string(trimmed[:windowLen]))
	return strings.HasPrefix(lower, "solid") &&
		strings.Contains(lower, "facet") &&
		strings.Contains(lower, "vertex")
}

// errInvalid wraps an ErrorCode so callers can classify errors with
// errors.As / errors.Is style checks if desired, while fmt.Errorf("%w", ...)
// still composes a human-readable message.
type engineError struct {
	Code ErrorCode
}

func (e *engineError) Error() string { return string(e.Code) }

func errInvalid(code ErrorCode) error { return &engineError{Code: code} }

// CodeOf extracts the ErrorCode from an error produced by this package, or
// ErrInternal if the error did not originate here.
func CodeOf(err error) ErrorCode {
	if err == nil {
		return ""
	}
	var ee *engineError
	if asEngineError(err, &ee) {
		return ee.Code
	}
	return ErrInternal
}

// asEngineError is a tiny local errors.As to avoid importing "errors" twice
// across files for one helper; kept explicit for clarity.
func asEngineError(err error, target **engineError) bool {
	for err != nil {
		if ee, ok := err.(*engineError); ok {
			*target = ee
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
