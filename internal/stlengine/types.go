// Package stlengine implements a streaming, hybrid STL analysis engine.
//
// Design summary (see /docs/ARCHITECTURE.md for the full explanation):
//
//	UPLOAD -> TEMP FILE -> FORMAT DETECTION -> STREAMING READER
//	       -> FAST O(N) VOLUME + BBOX + CHEAP VALIDATION
//	       -> (only if needed) COMPACT TOPOLOGY PASS for manifold/component info
//	       -> RESULT
//
// The engine never loads the whole file into RAM, never stores the full
// vertex/triangle list, and never mutates or re-derives the customer's
// original geometry. It only ever reads it.
package stlengine

import "math"

// Vec3 is a plain 3D point/vector. Always float64 for calculation accuracy,
// even though STL binary stores float32 — see reader_binary.go.
type Vec3 struct {
	X, Y, Z float64
}

// Sub returns a-b.
func (a Vec3) Sub(b Vec3) Vec3 {
	return Vec3{a.X - b.X, a.Y - b.Y, a.Z - b.Z}
}

// MeshStatus classifies the topological reliability of the analyzed mesh.
// This is returned to the API caller so the frontend can show a meaningful
// message instead of a raw failure.
type MeshStatus string

const (
	// StatusClosed: single watertight, consistently-oriented shell. Volume
	// is the physical solid volume.
	StatusClosed MeshStatus = "closed"
	// StatusMultiShell: more than one watertight shell (e.g. several parts
	// in one file, or a solid with an internal cavity). Volume accounts for
	// shell nesting (cavities are subtracted, not added).
	StatusMultiShell MeshStatus = "multi_shell"
	// StatusOpen: at least one boundary edge was found (the surface has a
	// hole). The signed-volume result is not a reliable physical volume.
	StatusOpen MeshStatus = "open"
	// StatusNonManifold: an edge is shared by more than two triangles, or
	// two triangles cover an edge with the same winding direction instead
	// of opposite directions (inconsistent normals).
	StatusNonManifold MeshStatus = "non_manifold"
	// StatusSuspicious: passed the cheap checks but the topology pass could
	// not be completed with confidence (e.g. mixed signals, or skipped
	// because the model exceeded the topology memory budget).
	StatusSuspicious MeshStatus = "suspicious"
	// StatusEmpty: no usable triangles were found.
	StatusEmpty MeshStatus = "empty"
	// StatusUnsupported: the file could not be parsed as STL at all.
	StatusUnsupported MeshStatus = "unsupported"
)

// ErrorCode is a machine-readable failure reason for API responses.
type ErrorCode string

const (
	ErrInvalidFile        ErrorCode = "INVALID_FILE"
	ErrFileTooLarge       ErrorCode = "FILE_TOO_LARGE"
	ErrInvalidBinarySTL   ErrorCode = "INVALID_BINARY_STL"
	ErrInvalidASCIISTL    ErrorCode = "INVALID_ASCII_STL"
	ErrTruncatedSTL       ErrorCode = "TRUNCATED_STL"
	ErrEmptyModel         ErrorCode = "EMPTY_MODEL"
	ErrInvalidCoordinates ErrorCode = "INVALID_COORDINATES"
	ErrOpenMesh           ErrorCode = "OPEN_MESH"
	ErrNonManifold        ErrorCode = "NON_MANIFOLD"
	ErrSelfIntersection   ErrorCode = "SELF_INTERSECTION_SUSPECTED"
	ErrVolumeUnreliable   ErrorCode = "VOLUME_UNRELIABLE"
	ErrProcessingTimeout  ErrorCode = "PROCESSING_TIMEOUT"
	ErrInternal           ErrorCode = "INTERNAL_ERROR"
)

// BoundingBox tracks min/max extents. Updated incrementally, O(1) per point.
type BoundingBox struct {
	Min, Max Vec3
	set      bool
}

// Add extends the box to include p.
func (b *BoundingBox) Add(p Vec3) {
	if !b.set {
		b.Min, b.Max, b.set = p, p, true
		return
	}
	if p.X < b.Min.X {
		b.Min.X = p.X
	}
	if p.Y < b.Min.Y {
		b.Min.Y = p.Y
	}
	if p.Z < b.Min.Z {
		b.Min.Z = p.Z
	}
	if p.X > b.Max.X {
		b.Max.X = p.X
	}
	if p.Y > b.Max.Y {
		b.Max.Y = p.Y
	}
	if p.Z > b.Max.Z {
		b.Max.Z = p.Z
	}
}

// Merge extends b to also cover o.
func (b *BoundingBox) Merge(o BoundingBox) {
	if !o.set {
		return
	}
	b.Add(o.Min)
	b.Add(o.Max)
}

// IsSet reports whether at least one point has been added.
func (b BoundingBox) IsSet() bool { return b.set }

// Size returns the (x,y,z) extents.
func (b BoundingBox) Size() Vec3 {
	return Vec3{b.Max.X - b.Min.X, b.Max.Y - b.Min.Y, b.Max.Z - b.Min.Z}
}

// Volume returns the axis-aligned bounding box volume (not the mesh volume).
func (b BoundingBox) Volume() float64 {
	s := b.Size()
	return s.X * s.Y * s.Z
}

// Contains reports whether o is fully inside b, with a small epsilon to
// absorb floating point noise between two independently-quantized shells.
func (b BoundingBox) Contains(o BoundingBox) bool {
	const eps = 1e-4
	return o.Min.X >= b.Min.X-eps && o.Max.X <= b.Max.X+eps &&
		o.Min.Y >= b.Min.Y-eps && o.Max.Y <= b.Max.Y+eps &&
		o.Min.Z >= b.Min.Z-eps && o.Max.Z <= b.Max.Z+eps
}

// Overlaps reports whether b and o share any volume.
func (b BoundingBox) Overlaps(o BoundingBox) bool {
	return b.Min.X <= o.Max.X && b.Max.X >= o.Min.X &&
		b.Min.Y <= o.Max.Y && b.Max.Y >= o.Min.Y &&
		b.Min.Z <= o.Max.Z && b.Max.Z >= o.Min.Z
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func pointFinite(p Vec3) bool {
	return isFinite(p.X) && isFinite(p.Y) && isFinite(p.Z)
}

// Options controls analysis behavior. Zero value is invalid; use
// DefaultOptions() and override fields as needed.
type Options struct {
	// MaxTriangles rejects files above this triangle count outright (0 =
	// use DefaultMaxTriangles). This is a hard safety cap, independent of
	// the topology budget below.
	MaxTriangles uint64

	// TopologyBudget is the maximum triangle count for which the engine
	// will build the compact per-edge / per-vertex topology structures
	// needed for manifold, open-edge and component detection. Above this
	// count, volume/dimensions are still computed exactly (that part is
	// O(1) memory), but mesh_status degrades to "suspicious" with
	// volume_reliable left as a best-effort flag from the cheap checks
	// only. This exists because topology tracking is inherently O(edges)
	// memory (~24-32 bytes/edge here); see ARCHITECTURE.md for the
	// reasoning. 0 = use DefaultTopologyBudget.
	TopologyBudget uint64

	// VertexWeldEpsilon is the absolute distance (in input units, i.e. mm)
	// below which two vertices are considered the same point for topology
	// purposes ONLY. It never affects the volume calculation, which always
	// uses exact input coordinates. 0 = use DefaultWeldEpsilon.
	VertexWeldEpsilon float64
}

const (
	DefaultMaxTriangles   uint64  = 20_000_000
	DefaultTopologyBudget uint64  = 6_000_000
	DefaultWeldEpsilon    float64 = 1e-4 // 0.1 micron; far finer than any FDM/SLA process
)

// DefaultOptions returns sane production defaults.
func DefaultOptions() Options {
	return Options{
		MaxTriangles:      DefaultMaxTriangles,
		TopologyBudget:    DefaultTopologyBudget,
		VertexWeldEpsilon: DefaultWeldEpsilon,
	}
}

func (o Options) withDefaults() Options {
	if o.MaxTriangles == 0 {
		o.MaxTriangles = DefaultMaxTriangles
	}
	if o.TopologyBudget == 0 {
		o.TopologyBudget = DefaultTopologyBudget
	}
	if o.VertexWeldEpsilon == 0 {
		o.VertexWeldEpsilon = DefaultWeldEpsilon
	}
	return o
}

// ComponentInfo describes one connected shell.
type ComponentInfo struct {
	TriangleCount uint64
	VolumeMM3     float64 // unsigned physical volume of this shell alone
	Bounds        BoundingBox
	IsCavity      bool // true if this shell is nested inside another and
	// therefore represents a void, not solid material
}

// Result is the outcome of analyzing an STL stream.
type Result struct {
	Format        string // "binary" or "ascii"
	TriangleCount uint64
	Dimensions    BoundingBox

	VolumeMM3      float64 // final physical volume (cavities already subtracted)
	VolumeReliable bool
	MeshStatus     MeshStatus
	Components     []ComponentInfo

	TopologyEvaluated bool // false if skipped due to TopologyBudget

	// Cheap-check counters, useful for diagnostics/logging.
	NonFiniteTriangles   uint64
	ZeroAreaTriangles    uint64
	DegenerateTriangles  uint64

	ErrorCode    ErrorCode
	ErrorMessage string
}

// Parsed reports whether the file could be read and measured at all. This
// can be true even when VolumeReliable is false — e.g. an open mesh parses
// fine and gets exact dimensions, but its signed-volume figure cannot be
// trusted as a physical volume. Callers building an HTTP response should
// branch on Parsed first, then decide what to do with VolumeReliable /
// MeshStatus / ErrorCode for the diagnostic payload.
func (r Result) Parsed() bool {
	return r.MeshStatus != "" && r.MeshStatus != StatusUnsupported
}
