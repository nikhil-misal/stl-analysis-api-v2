package stlengine

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// asciiTokenizer turns a byte stream into whitespace-delimited tokens
// without ever materializing a full line or the full file. This avoids two
// real problems with line-oriented parsing (bufio.Scanner + strings.Fields):
//
//  1. bufio.Scanner has a default 64KiB per-token (here: per-line) limit.
//     Some ASCII STL exporters put unusual whitespace or put multiple
//     "vertex" records on one very long line; a naive Scanner setup can
//     simply error out on those files ("token too long") even though the
//     model itself is small.
//  2. Splitting on lines and then calling strings.Fields per line still
//     scans each line twice and allocates a []string per line. A single
//     byte-oriented tokenizer sharing one reusable buffer avoids both.
type asciiTokenizer struct {
	r   *bufio.Reader
	buf []byte
}

func newASCIITokenizer(r io.Reader) *asciiTokenizer {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 1<<20) // 1 MiB
	}
	return &asciiTokenizer{r: br, buf: make([]byte, 0, 32)}
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f' || b == '\v'
}

// next returns the next whitespace-delimited token (lower-cased is NOT
// applied here — callers decide), or io.EOF once the stream is exhausted.
func (t *asciiTokenizer) next() (string, error) {
	// skip leading whitespace
	var b byte
	var err error
	for {
		b, err = t.r.ReadByte()
		if err != nil {
			return "", err
		}
		if !isSpaceByte(b) {
			break
		}
	}
	t.buf = t.buf[:0]
	t.buf = append(t.buf, b)
	for {
		b, err = t.r.ReadByte()
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", err
		}
		if isSpaceByte(b) {
			break
		}
		t.buf = append(t.buf, b)
	}
	return string(t.buf), nil
}

// streamASCIISTL scans r for "vertex X Y Z" triples, grouping every three
// into one triangle and invoking visit. It intentionally ignores
// "solid"/"facet normal"/"outer loop"/"endloop"/"endfacet"/"endsolid"
// structural keywords, the same tolerant approach the existing parser uses,
// just token-based and streaming instead of line-based and pre-loaded.
func streamASCIISTL(r io.Reader, visit visitFunc) (triangleCount uint64, err error) {
	tok := newASCIITokenizer(r)
	var verts [3]Vec3
	have := 0
	var pendingXYZ [3]float64

	for {
		word, terr := tok.next()
		if terr != nil {
			if terr == io.EOF {
				break
			}
			return triangleCount, terr
		}
		if !strings.EqualFold(word, "vertex") {
			continue
		}
		for i := 0; i < 3; i++ {
			numTok, terr := tok.next()
			if terr != nil {
				return triangleCount, fmt.Errorf("%w: ASCII STL ended mid-vertex", errInvalid(ErrInvalidASCIISTL))
			}
			v, perr := strconv.ParseFloat(numTok, 64)
			if perr != nil {
				return triangleCount, fmt.Errorf("%w: invalid coordinate %q", errInvalid(ErrInvalidCoordinates), numTok)
			}
			pendingXYZ[i] = v
		}
		verts[have] = Vec3{pendingXYZ[0], pendingXYZ[1], pendingXYZ[2]}
		have++
		if have == 3 {
			if err := visit(verts[0], verts[1], verts[2]); err != nil {
				return triangleCount, err
			}
			triangleCount++
			have = 0
		}
	}

	// Note: an empty result (triangleCount == 0) is intentionally NOT
	// treated as an error here — Analyze()'s post-stream check handles that
	// uniformly for both binary and ASCII so the reported MeshStatus/ErrorCode
	// is consistent regardless of which reader produced zero triangles.
	return triangleCount, nil
}
