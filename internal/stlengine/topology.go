package stlengine

// topology.go implements the ONLY part of this engine that needs more than
// O(1) extra memory: figuring out whether the surface is closed, whether
// any edge is non-manifold, and how many separate shells exist.
//
// This is fundamentally an O(edges) problem — you cannot know an edge is
// shared by exactly two triangles without counting, for every edge, how
// many triangles touched it. What we control is HOW MUCH each edge costs.
// The previous implementation kept, per edge, a growing []int of owning
// triangles and a parallel []int of directions. For a 500k-triangle mesh
// (~750k edges) that is millions of small heap-allocated slices. Here each
// edge costs one fixed-size struct value (edgeState, 16 bytes) stored
// directly in a map — no nested slices, no per-triangle allocations, and
// no separate adjacency graph.
//
// Component membership is tracked with weighted union-find over quantized
// VERTEX identity (not a full vertex table): two triangles are considered
// connected if they share a vertex. That is sufficient for STL meshes,
// where shells are joined through shared triangle vertices/edges, and it
// costs one map entry per unique quantized vertex rather than one entry per
// unique 3D point PLUS a duplicate slice of all points (which is what the
// existing vertex-welding builder keeps for every format today).
//
// Both structures share the same quantization keys and are updated in the
// SAME triangle loop as the volume/bbox pass — no second full pass over the
// triangle stream is needed to get this information.

type edgeDir int8

const (
	dirForward  edgeDir = 1
	dirBackward edgeDir = -1
)

// edgeKey identifies an undirected edge by its two (quantized) endpoints,
// normalized so the numerically smaller key comes first. That normalization
// is what lets us detect orientation: a triangle edge (u -> v) contributes
// +1 if u came first in the normalized key, or -1 if v came first. A
// correctly, consistently oriented closed manifold visits every edge
// exactly once in each direction, so the two contributions cancel to 0.
type edgeKey struct {
	A, B vKey
}

func makeEdgeKey(u, v vKey) (edgeKey, edgeDir) {
	if less(u, v) {
		return edgeKey{u, v}, dirForward
	}
	return edgeKey{v, u}, dirBackward
}

func less(a, b vKey) bool {
	if a.X != b.X {
		return a.X < b.X
	}
	if a.Y != b.Y {
		return a.Y < b.Y
	}
	return a.Z < b.Z
}

// edgeState is the compact per-edge accumulator. 2 int32 fields = 8 bytes,
// versus the reference implementation's per-edge []int slices.
type edgeState struct {
	Count int32 // number of triangles that touched this edge
	Dir   int32 // signed sum of dirForward/dirBackward contributions
}

// componentRecord aggregates everything needed about one connected shell.
// Kept small and merged in-place during union() so we never need a second
// pass over triangles to compute per-component totals.
type componentRecord struct {
	triangles uint64
	volumeSum neumaierSum
	bounds    BoundingBox
}

// topologyTracker owns the union-find forest and the edge map. It is only
// allocated when a file is within Options.TopologyBudget.
type topologyTracker struct {
	origin Vec3
	eps    float64

	// union-find over vertex identity
	parent map[vKey]vKey
	rank   map[vKey]uint8
	// aggregate data lives at the CURRENT root of each tree and is merged
	// into the surviving root whenever two trees are unioned.
	records map[vKey]*componentRecord

	edges map[edgeKey]edgeState

	uniqueVertices uint64
}

func newTopologyTracker(origin Vec3, eps float64) *topologyTracker {
	return &topologyTracker{
		origin:  origin,
		eps:     eps,
		parent:  make(map[vKey]vKey),
		rank:    make(map[vKey]uint8),
		records: make(map[vKey]*componentRecord),
		edges:   make(map[edgeKey]edgeState),
	}
}

func (t *topologyTracker) find(k vKey) vKey {
	root := k
	for {
		p, ok := t.parent[root]
		if !ok || p == root {
			break
		}
		root = p
	}
	// path compression
	for k != root {
		next := t.parent[k]
		t.parent[k] = root
		k = next
	}
	return root
}

func (t *topologyTracker) ensure(k vKey) {
	if _, ok := t.parent[k]; ok {
		return
	}
	t.parent[k] = k
	t.rank[k] = 0
	t.records[k] = &componentRecord{}
	t.uniqueVertices++
}

// union merges the trees containing a and b, merging their aggregate
// records into the surviving root, and returns the surviving root.
func (t *topologyTracker) union(a, b vKey) vKey {
	ra, rb := t.find(a), t.find(b)
	if ra == rb {
		return ra
	}
	// union by rank
	if t.rank[ra] < t.rank[rb] {
		ra, rb = rb, ra
	} else if t.rank[ra] == t.rank[rb] {
		t.rank[ra]++
	}
	t.parent[rb] = ra

	survivor := t.records[ra]
	absorbed := t.records[rb]
	survivor.triangles += absorbed.triangles
	survivor.volumeSum.Add(absorbed.volumeSum.Value())
	survivor.bounds.Merge(absorbed.bounds)
	delete(t.records, rb)

	return ra
}

// addTriangle folds one triangle into both the union-find component data
// and the edge-parity map. a,b,c are the RAW (non-quantized) vertex
// coordinates; volume6 is the already-computed signedTetraVolume6(origin,
// a,b,c) contribution for this triangle so we don't recompute it.
func (t *topologyTracker) addTriangle(a, b, c Vec3, volume6 float64, bbox BoundingBox) {
	ka := quantize(a, t.origin, t.eps)
	kb := quantize(b, t.origin, t.eps)
	kc := quantize(c, t.origin, t.eps)

	t.ensure(ka)
	t.ensure(kb)
	t.ensure(kc)

	root := t.union(ka, kb)
	root = t.union(root, kc)

	rec := t.records[root]
	rec.triangles++
	rec.volumeSum.Add(volume6 / 6.0)
	rec.bounds.Merge(bbox)

	t.addEdge(ka, kb)
	t.addEdge(kb, kc)
	t.addEdge(kc, ka)
}

func (t *topologyTracker) addEdge(u, v vKey) {
	if u == v {
		return // degenerate edge, already flagged separately as a bad triangle
	}
	key, dir := makeEdgeKey(u, v)
	st := t.edges[key]
	st.Count++
	st.Dir += int32(dir)
	t.edges[key] = st
}

// manifoldSummary is the result of scanning the accumulated edge map once,
// at the end of processing.
type manifoldSummary struct {
	OpenEdges        uint64 // edges touched by exactly 1 triangle
	NonManifoldEdges uint64 // edges touched by 3+ triangles
	FlippedEdges     uint64 // edges touched by exactly 2 triangles with the SAME direction (inconsistent winding)
}

func (t *topologyTracker) summarizeManifold() manifoldSummary {
	var s manifoldSummary
	for _, st := range t.edges {
		switch {
		case st.Count == 1:
			s.OpenEdges++
		case st.Count == 2:
			if st.Dir != 0 {
				s.FlippedEdges++
			}
		case st.Count >= 3:
			s.NonManifoldEdges++
		}
	}
	return s
}

// components returns the final per-shell aggregates, resolving every vertex
// to its current (post path-compression-friendly) root on demand.
func (t *topologyTracker) components() []ComponentInfo {
	out := make([]ComponentInfo, 0, len(t.records))
	for root, rec := range t.records {
		_ = root
		if rec.triangles == 0 {
			continue
		}
		out = append(out, ComponentInfo{
			TriangleCount: rec.triangles,
			VolumeMM3:     absFloat(rec.volumeSum.Value()),
			Bounds:        rec.bounds,
		})
	}
	return out
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
