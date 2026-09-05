package stlengine

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// visitFunc receives one triangle's three vertices, in file order. Readers
// call it once per triangle; callers must not retain the Vec3 values beyond
// the call (they're passed by value, so that's automatic — this comment is
// just documenting intent).
type visitFunc func(a, b, c Vec3) error

// streamBinarySTL reads exactly `count` 50-byte triangle records from r
// (which must already be positioned just after the 84-byte header) and
// invokes visit for each one.
//
// Performance notes:
//   - A single [50]byte buffer is reused for every triangle. No per-triangle
//     allocation.
//   - r is wrapped in a bufio.Reader sized for good throughput on both local
//     disk and network-backed temp storage; io.ReadFull pulls exactly 50
//     bytes per call regardless of how the underlying reader chooses to
//     fragment its Read() calls.
//   - The normal vector (bytes 0-11 of each record) is skipped — the fast
//     path derives everything it needs from vertex positions.
func streamBinarySTL(r io.Reader, count uint32, visit visitFunc) error {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 1<<20) // 1 MiB
	}

	var buf [binaryTriangleRecordSize]byte
	for i := uint32(0); i < count; i++ {
		if _, err := io.ReadFull(br, buf[:]); err != nil {
			if err == io.ErrUnexpectedEOF || err == io.EOF {
				return fmt.Errorf("%w: binary STL ended after %d of %d triangles",
					errInvalid(ErrTruncatedSTL), i, count)
			}
			return err
		}
		a := decodeVec3(buf[12:24])
		b := decodeVec3(buf[24:36])
		c := decodeVec3(buf[36:48])
		if err := visit(a, b, c); err != nil {
			return err
		}
	}
	return nil
}

func decodeVec3(b []byte) Vec3 {
	return Vec3{
		X: float64(math.Float32frombits(binary.LittleEndian.Uint32(b[0:4]))),
		Y: float64(math.Float32frombits(binary.LittleEndian.Uint32(b[4:8]))),
		Z: float64(math.Float32frombits(binary.LittleEndian.Uint32(b[8:12]))),
	}
}
