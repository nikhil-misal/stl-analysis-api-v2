package stlengine

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------
// Synthetic geometry generators (no external test fixtures required)
// ---------------------------------------------------------------------

// cubeFaceCorners returns each of the 6 unit-cube faces as 4 corners, in an
// order that has already been verified (see ARCHITECTURE.md) to produce
// consistent outward-facing CCW winding when split into two triangles
// (corners[0],corners[1],corners[2]) and (corners[0],corners[2],corners[3]).
func cubeFaceCorners() [6][4]Vec3 {
	return [6][4]Vec3{
		{{0, 0, 1}, {1, 0, 1}, {1, 1, 1}, {0, 1, 1}}, // +z
		{{0, 0, 0}, {0, 1, 0}, {1, 1, 0}, {1, 0, 0}}, // -z
		{{0, 1, 0}, {0, 1, 1}, {1, 1, 1}, {1, 1, 0}}, // +y
		{{0, 0, 0}, {1, 0, 0}, {1, 0, 1}, {0, 0, 1}}, // -y
		{{1, 0, 0}, {1, 1, 0}, {1, 1, 1}, {1, 0, 1}}, // +x
		{{0, 0, 0}, {0, 0, 1}, {0, 1, 1}, {0, 1, 0}}, // -x
	}
}

// tessellateCube returns a closed, manifold, consistently-oriented cube of
// the given edge length, translated by offset, with each face subdivided
// into an n x n grid (2*n^2 triangles per face, 12*n^2 total). n=1
// reproduces the minimal 12-triangle cube. The enclosed volume is exactly
// size^3 regardless of n.
func tessellateCube(size float64, offset Vec3, n int) [][3]Vec3 {
	if n < 1 {
		n = 1
	}
	lerp := func(a, b Vec3, t float64) Vec3 {
		return Vec3{a.X + (b.X-a.X)*t, a.Y + (b.Y-a.Y)*t, a.Z + (b.Z-a.Z)*t}
	}
	bilerp := func(c [4]Vec3, u, v float64) Vec3 {
		top := lerp(c[0], c[1], u)
		bot := lerp(c[3], c[2], u)
		return lerp(top, bot, v)
	}
	scale := func(p Vec3) Vec3 {
		return Vec3{p.X*size + offset.X, p.Y*size + offset.Y, p.Z*size + offset.Z}
	}

	tris := make([][3]Vec3, 0, 12*n*n)
	for _, corners := range cubeFaceCorners() {
		for i := 0; i < n; i++ {
			for j := 0; j < n; j++ {
				u0, u1 := float64(i)/float64(n), float64(i+1)/float64(n)
				v0, v1 := float64(j)/float64(n), float64(j+1)/float64(n)
				p00 := scale(bilerp(corners, u0, v0))
				p10 := scale(bilerp(corners, u1, v0))
				p11 := scale(bilerp(corners, u1, v1))
				p01 := scale(bilerp(corners, u0, v1))
				tris = append(tris, [3]Vec3{p00, p10, p11})
				tris = append(tris, [3]Vec3{p00, p11, p01})
			}
		}
	}
	return tris
}

func buildBinarySTL(tris [][3]Vec3) []byte {
	buf := make([]byte, 84+50*len(tris))
	binary.LittleEndian.PutUint32(buf[80:84], uint32(len(tris)))
	off := 84
	for _, t := range tris {
		rec := buf[off : off+50]
		putVec3F32(rec[12:24], t[0])
		putVec3F32(rec[24:36], t[1])
		putVec3F32(rec[36:48], t[2])
		off += 50
	}
	return buf
}

func putVec3F32(b []byte, v Vec3) {
	binary.LittleEndian.PutUint32(b[0:4], math.Float32bits(float32(v.X)))
	binary.LittleEndian.PutUint32(b[4:8], math.Float32bits(float32(v.Y)))
	binary.LittleEndian.PutUint32(b[8:12], math.Float32bits(float32(v.Z)))
}

func buildASCIISTL(tris [][3]Vec3) []byte {
	var sb strings.Builder
	sb.WriteString("solid test\n")
	for _, t := range tris {
		sb.WriteString("facet normal 0 0 0\nouter loop\n")
		for _, v := range t {
			fmt.Fprintf(&sb, "vertex %v %v %v\n", v.X, v.Y, v.Z)
		}
		sb.WriteString("endloop\nendfacet\n")
	}
	sb.WriteString("endsolid test\n")
	return []byte(sb.String())
}

func analyzeBytes(t *testing.T, data []byte, opts Options) Result {
	t.Helper()
	r := bytes.NewReader(data)
	res, err := Analyze(context.Background(), r, int64(len(data)), opts)
	if err != nil && res.MeshStatus == "" {
		t.Fatalf("Analyze returned hard error with no result: %v", err)
	}
	return res
}

const volTolerance = 1e-6 // relative

func approxEqual(a, b, relTol float64) bool {
	if b == 0 {
		return math.Abs(a) < 1e-9
	}
	return math.Abs(a-b)/math.Abs(b) < relTol
}

// ---------------------------------------------------------------------
// Golden tests
// ---------------------------------------------------------------------

func TestCubeBinaryVolumeAndDimensions(t *testing.T) {
	tris := tessellateCube(10, Vec3{}, 1)
	data := buildBinarySTL(tris)
	res := analyzeBytes(t, data, DefaultOptions())

	if res.ErrorCode != "" {
		t.Fatalf("unexpected error: %s (%s)", res.ErrorCode, res.ErrorMessage)
	}
	if res.MeshStatus != StatusClosed {
		t.Fatalf("expected closed mesh, got %s", res.MeshStatus)
	}
	if !res.VolumeReliable {
		t.Fatalf("expected reliable volume")
	}
	if !approxEqual(res.VolumeMM3, 1000, volTolerance) {
		t.Fatalf("expected ~1000 mm3, got %v", res.VolumeMM3)
	}
	size := res.Dimensions.Size()
	for _, v := range []float64{size.X, size.Y, size.Z} {
		if !approxEqual(v, 10, volTolerance) {
			t.Fatalf("expected 10mm dimensions, got %+v", size)
		}
	}
	if res.TriangleCount != uint64(len(tris)) {
		t.Fatalf("expected %d triangles, got %d", len(tris), res.TriangleCount)
	}
}

func TestCubeASCIIVolume(t *testing.T) {
	tris := tessellateCube(10, Vec3{}, 1)
	data := buildASCIISTL(tris)
	res := analyzeBytes(t, data, DefaultOptions())

	if res.ErrorCode != "" {
		t.Fatalf("unexpected error: %s (%s)", res.ErrorCode, res.ErrorMessage)
	}
	if res.Format != "ascii" {
		t.Fatalf("expected ascii format, got %s", res.Format)
	}
	if !approxEqual(res.VolumeMM3, 1000, volTolerance) {
		t.Fatalf("expected ~1000 mm3, got %v", res.VolumeMM3)
	}
}

func TestCubeTranslatedToLargeCoordinates(t *testing.T) {
	// Same 10mm cube, but the entire model sits a million mm away from the
	// world origin. The physical volume must not degrade.
	tris := tessellateCube(10, Vec3{X: 1_000_000, Y: 2_000_000, Z: 3_000_000}, 2)
	data := buildBinarySTL(tris)
	res := analyzeBytes(t, data, DefaultOptions())

	if res.ErrorCode != "" {
		t.Fatalf("unexpected error: %s (%s)", res.ErrorCode, res.ErrorMessage)
	}
	if !res.VolumeReliable {
		t.Fatalf("expected reliable volume")
	}
	// float32 STL storage means large absolute coordinates only carry ~7
	// significant digits, so allow a looser (but still tight) tolerance
	// than the origin-centered case.
	if !approxEqual(res.VolumeMM3, 1000, 1e-2) {
		t.Fatalf("expected ~1000 mm3 even far from the origin, got %v", res.VolumeMM3)
	}
}

func TestTessellatedCubeStillExact(t *testing.T) {
	// A finer mesh of the same solid should not change the volume — this
	// guards against any per-triangle bias in the accumulation.
	tris := tessellateCube(25, Vec3{}, 12) // 12*12*12 = 1728 triangles
	data := buildBinarySTL(tris)
	res := analyzeBytes(t, data, DefaultOptions())

	if res.MeshStatus != StatusClosed || !res.VolumeReliable {
		t.Fatalf("expected closed+reliable, got status=%s reliable=%v msg=%s",
			res.MeshStatus, res.VolumeReliable, res.ErrorMessage)
	}
	want := 25.0 * 25.0 * 25.0
	if !approxEqual(res.VolumeMM3, want, volTolerance) {
		t.Fatalf("expected %v mm3, got %v", want, res.VolumeMM3)
	}
}

func TestOpenMeshMissingFacet(t *testing.T) {
	tris := tessellateCube(10, Vec3{}, 1)
	tris = tris[:len(tris)-1] // drop one triangle -> one boundary loop
	data := buildBinarySTL(tris)
	res := analyzeBytes(t, data, DefaultOptions())

	if res.MeshStatus != StatusOpen {
		t.Fatalf("expected open mesh, got %s (reliable=%v)", res.MeshStatus, res.VolumeReliable)
	}
	if res.VolumeReliable {
		t.Fatalf("open mesh must not be reported as reliable")
	}
	if res.ErrorCode != ErrOpenMesh {
		t.Fatalf("expected OPEN_MESH, got %s", res.ErrorCode)
	}
}

func TestNonManifoldDuplicatedFacet(t *testing.T) {
	tris := tessellateCube(10, Vec3{}, 1)
	tris = append(tris, tris[0]) // one edge now touched by 3 triangles
	data := buildBinarySTL(tris)
	res := analyzeBytes(t, data, DefaultOptions())

	if res.MeshStatus != StatusNonManifold {
		t.Fatalf("expected non-manifold, got %s", res.MeshStatus)
	}
	if res.VolumeReliable {
		t.Fatalf("non-manifold mesh must not be reported as reliable")
	}
}

func TestMultiShellTwoDisjointCubes(t *testing.T) {
	a := tessellateCube(10, Vec3{}, 1)
	b := tessellateCube(10, Vec3{X: 1000}, 1) // far away, no shared vertices
	tris := append(append([][3]Vec3{}, a...), b...)
	data := buildBinarySTL(tris)
	res := analyzeBytes(t, data, DefaultOptions())

	if res.MeshStatus != StatusMultiShell {
		t.Fatalf("expected multi_shell, got %s", res.MeshStatus)
	}
	if !res.VolumeReliable {
		t.Fatalf("expected reliable volume for two clean separate solids")
	}
	if len(res.Components) != 2 {
		t.Fatalf("expected 2 components, got %d", len(res.Components))
	}
	if !approxEqual(res.VolumeMM3, 2000, volTolerance) {
		t.Fatalf("expected 2000 mm3 (two 10mm cubes), got %v", res.VolumeMM3)
	}
	for _, c := range res.Components {
		if c.IsCavity {
			t.Fatalf("neither disjoint cube should be classified as a cavity")
		}
	}
}

func TestNestedCavityIsSubtracted(t *testing.T) {
	outer := tessellateCube(20, Vec3{}, 1)
	inner := tessellateCube(10, Vec3{X: 5, Y: 5, Z: 5}, 1) // fully inside outer
	tris := append(append([][3]Vec3{}, outer...), inner...)
	data := buildBinarySTL(tris)
	res := analyzeBytes(t, data, DefaultOptions())

	if res.MeshStatus != StatusMultiShell {
		t.Fatalf("expected multi_shell, got %s", res.MeshStatus)
	}
	if !res.VolumeReliable {
		t.Fatalf("expected reliable volume, got message: %s", res.ErrorMessage)
	}
	want := 20.0*20.0*20.0 - 10.0*10.0*10.0
	if !approxEqual(res.VolumeMM3, want, volTolerance) {
		t.Fatalf("expected %v mm3 (outer minus cavity), got %v", want, res.VolumeMM3)
	}
	cavities := 0
	for _, c := range res.Components {
		if c.IsCavity {
			cavities++
		}
	}
	if cavities != 1 {
		t.Fatalf("expected exactly 1 shell classified as a cavity, got %d", cavities)
	}
}

func TestTruncatedBinarySTL(t *testing.T) {
	tris := tessellateCube(10, Vec3{}, 1)
	data := buildBinarySTL(tris)
	truncated := data[:len(data)-25] // cut off mid last triangle
	res := analyzeBytes(t, truncated, DefaultOptions())

	if res.ErrorCode != ErrTruncatedSTL {
		t.Fatalf("expected TRUNCATED_STL, got %s (%s)", res.ErrorCode, res.ErrorMessage)
	}
}

func TestEmptyBinarySTL(t *testing.T) {
	data := buildBinarySTL(nil) // valid 84-byte header, 0 triangles
	res := analyzeBytes(t, data, DefaultOptions())

	if res.ErrorCode != ErrEmptyModel {
		t.Fatalf("expected EMPTY_MODEL, got %s", res.ErrorCode)
	}
}

func TestNaNCoordinateIsFlagged(t *testing.T) {
	tris := tessellateCube(10, Vec3{}, 1)
	tris[0][0] = Vec3{X: math.NaN(), Y: 0, Z: 0}
	data := buildBinarySTL(tris)
	res := analyzeBytes(t, data, DefaultOptions())

	if res.VolumeReliable {
		t.Fatalf("a model with a NaN vertex must never report a reliable volume")
	}
	if res.NonFiniteTriangles == 0 {
		t.Fatalf("expected at least one non-finite triangle to be counted")
	}
	if res.ErrorCode != ErrInvalidCoordinates {
		t.Fatalf("expected INVALID_COORDINATES, got %s", res.ErrorCode)
	}
}

func TestBinaryDetectionIgnoresHeaderText(t *testing.T) {
	// A binary STL whose 80-byte header happens to literally start with the
	// word "solid" must still be detected as binary, because the size
	// formula matches exactly. This is the classic false-ASCII-detection
	// bug this engine specifically avoids.
	tris := tessellateCube(5, Vec3{}, 1)
	data := buildBinarySTL(tris)
	copy(data[0:5], []byte("solid"))
	res := analyzeBytes(t, data, DefaultOptions())

	if res.Format != "binary" {
		t.Fatalf("expected binary despite 'solid' header text, got %s", res.Format)
	}
	if res.ErrorCode != "" {
		t.Fatalf("unexpected error: %s", res.ErrorCode)
	}
}

func TestASCIIWithUnusualWhitespaceAndCase(t *testing.T) {
	raw := "SOLID weird\r\n  FACET   NORMAL 0 0 0\nouter\tloop\n" +
		"VERTEX 0 0 0\nvertex 10   0 0\nVeRtEx 0 10 0\nendloop\nendfacet\n" +
		"facet normal 0 0 0\nouter loop\nvertex 0 0 0\nvertex 0 10 0\nvertex 0 0 10\n" +
		"endloop\nendfacet\nendsolid\n"
	res := analyzeBytes(t, []byte(raw), DefaultOptions())
	// This tiny 2-triangle open fan isn't watertight, so we only assert
	// that it PARSED (didn't hit a hard error) rather than checking volume.
	if res.MeshStatus == "" || res.MeshStatus == StatusUnsupported {
		t.Fatalf("expected the tokenizer to tolerate mixed case/whitespace, got status=%s err=%s",
			res.MeshStatus, res.ErrorMessage)
	}
	if res.TriangleCount != 2 {
		t.Fatalf("expected 2 triangles parsed, got %d", res.TriangleCount)
	}
}

func TestTopologyBudgetIsRespected(t *testing.T) {
	tris := tessellateCube(10, Vec3{}, 3) // 108 triangles
	data := buildBinarySTL(tris)
	opts := DefaultOptions()
	opts.TopologyBudget = 10 // force skipping topology analysis
	res := analyzeBytes(t, data, opts)

	if res.TopologyEvaluated {
		t.Fatalf("expected topology evaluation to be skipped above budget")
	}
	if res.VolumeReliable {
		t.Fatalf("volume must not be marked reliable when topology was skipped")
	}
	if !approxEqual(res.VolumeMM3, 1000, volTolerance) {
		t.Fatalf("volume/dimensions should still be exact even without topology, got %v", res.VolumeMM3)
	}
}

func TestMaxTrianglesHardCap(t *testing.T) {
	tris := tessellateCube(10, Vec3{}, 3) // 108 triangles
	data := buildBinarySTL(tris)
	opts := DefaultOptions()
	opts.MaxTriangles = 10
	res := analyzeBytes(t, data, opts)

	if res.ErrorCode != ErrFileTooLarge {
		t.Fatalf("expected FILE_TOO_LARGE, got %s", res.ErrorCode)
	}
}
