# STL Engine Upgrade — README

A drop-in, streaming, hybrid STL analysis engine for
`3d-print-slicer-api`. See `docs/ARCHITECTURE.md` for the full design
rationale and `docs/INTEGRATION.md` for exact merge steps. This file covers
day-to-day usage: environment variables, running it, and example
requests/responses.

## Why this exists

The existing `/analyze` endpoint reads the entire upload into memory, and
routes STL files through a general-purpose vertex-welding mesh builder plus
a topology function (`calculateRobustMeshVolume`) that — based on the
identifiers you described (`edgeMap`, `edgeUse{Triangles []int, Dirs
[]int}`, `pointInsideComponent`, `rayIntersectsTriangle`) — allocates a slice
per edge and appears to do per-component ray casting without a bounding-box
prefilter. That combination degrades badly as triangle count grows,
independent of whether the mesh is actually valid. This upgrade replaces the
STL-specific path only: format detection, streaming parse, volume,
dimensions, and manifold/component analysis, all in one O(N)-time pass with
O(1) memory for the normal case and a bounded, documented O(N) memory cost
only when full topology verification is requested. See
`docs/ARCHITECTURE.md` §1 for the full audit and §8 for a complexity table.

## Environment variables

| Variable | Default | Meaning |
|---|---|---|
| `MAX_UPLOAD_SIZE_MB` | `50` | Hard cap on upload size, enforced via `http.MaxBytesReader` before any parsing begins. |
| `MAX_CONCURRENT_ANALYSIS` | `4` | Max `/analyze` requests whose geometry pass runs at once; extra requests get an immediate `429` instead of queuing. |
| `ANALYSIS_TIMEOUT_SECONDS` | `30` | Per-request context deadline for the STL analysis pass. |
| `MAX_TRIANGLES` | `20000000` | Hard safety cap — files declaring more triangles than this are rejected before any triangle is read. This is a DoS guard, not a quality gate: legitimate multi-million-triangle files are expected to pass through it fine at the default. |
| `TOPOLOGY_BUDGET_TRIANGLES` | `6000000` | Above this triangle count, volume and dimensions are still computed exactly, but manifold/component verification is skipped and the response reports `mesh_status: "suspicious"`, `volume_reliable: false` instead. See `docs/ARCHITECTURE.md` §5 for why this can't be zero-cost at any size. |

All are optional; the service runs correctly with none of them set.

## Running locally

```bash
export MAX_UPLOAD_SIZE_MB=100
go run .
# service listens on :8080 (or $PORT)
```

## Running with Docker

Your existing `Dockerfile` needs no changes for this upgrade — no new
external dependencies were introduced (everything under `internal/` is
standard library only), and temp files are written via `os.CreateTemp("",
...)`, i.e. whatever `$TMPDIR`/`/tmp` the container already provides. Just
make sure the new environment variables above are passed through if you
want non-default values:

```bash
docker build -t 3d-print-slicer-api .
docker run -p 8080:8080 \
  -e MAX_UPLOAD_SIZE_MB=100 \
  -e MAX_CONCURRENT_ANALYSIS=8 \
  3d-print-slicer-api
```

## Example requests

### A normal, valid STL

```bash
curl -X POST http://localhost:8080/analyze \
  -F "file=@bracket.stl" \
  -F "material=PETG" \
  -F "infill=20"
```

```json
{
  "success": true,
  "volume_cm3": 12.345678,
  "solid_weight_g": 15.679,
  "estimated_print_weight_g": 6.271,
  "triangle_count": 4832,
  "vertex_count": 14496,
  "material": "PETG",
  "infill": 20,
  "file_type": "stl",
  "unit": "mm",
  "closed_mesh": true,
  "components": 1,
  "message": "Model analyzed successfully",
  "dimensions": { "x_mm": 42.1, "y_mm": 30.0, "z_mm": 18.6 },
  "mesh": {
    "status": "closed",
    "volume_reliable": true,
    "topology_evaluated": true,
    "components": 1,
    "cavity_components": 0
  },
  "file_hash_sha256": "9b3f...c1a2",
  "processing_time_ms": 41
}
```

### The known ~499,832-triangle complex file

Same request shape. With the default `TOPOLOGY_BUDGET_TRIANGLES=6000000`,
a file this size is comfortably inside the budget, so it gets full manifold
verification, not just a fast-path volume:

```json
{
  "success": true,
  "volume_cm3": 187.42,
  "triangle_count": 499832,
  "mesh": {
    "status": "closed",
    "volume_reliable": true,
    "topology_evaluated": true,
    "components": 1,
    "cavity_components": 0
  },
  "processing_time_ms": 620
}
```

(The exact number depends on the actual file — I was not given the file
itself, so this shape is illustrative; see the Benchmarks section below for
what to expect performance-wise at this scale using a synthetic mesh of the
same size.)

### An open (non-watertight) mesh

```json
{
  "success": false,
  "triangle_count": 812,
  "dimensions": { "x_mm": 20.0, "y_mm": 20.0, "z_mm": 20.0 },
  "mesh": {
    "status": "open",
    "volume_reliable": false,
    "topology_evaluated": true,
    "components": 1,
    "cavity_components": 0,
    "error_code": "OPEN_MESH"
  },
  "error": "Your 3D model could not be measured automatically because it isn't a closed, watertight shape. Please make sure the model has no holes and try again.",
  "file_hash_sha256": "1a2b...ff90",
  "processing_time_ms": 9
}
```

### A file that's too large for topology verification, but still measurable

```json
{
  "success": false,
  "triangle_count": 9000000,
  "dimensions": { "x_mm": 150.2, "y_mm": 140.0, "z_mm": 90.5 },
  "mesh": {
    "status": "suspicious",
    "volume_reliable": false,
    "topology_evaluated": false,
    "components": 0,
    "cavity_components": 0,
    "error_code": "VOLUME_UNRELIABLE"
  },
  "error": "This model's size could not be fully verified, so we can't guarantee an accurate price yet. A team member may need to review it.",
  "processing_time_ms": 1840
}
```

## Testing

```bash
go test ./internal/stlengine/...
```

Covers, using synthetically generated (in-memory, no fixture files needed)
watertight meshes:

- Binary and ASCII cube volume/dimensions (exact, golden values)
- The same cube translated 1,000,000mm from the origin (numerical stability)
- A finer tessellation of the same solid (volume must not drift)
- Open mesh (missing facet) → `mesh_status: "open"`, `volume_reliable: false`
- Non-manifold mesh (duplicated facet) → `mesh_status: "non_manifold"`
- Two disjoint solids → `mesh_status: "multi_shell"`, volumes summed
- A solid with a fully-nested cavity → cavity volume subtracted, not added
- Truncated binary STL → `TRUNCATED_STL`
- Empty STL (valid header, 0 triangles) → `EMPTY_MODEL`
- A triangle with a NaN coordinate → excluded, `volume_reliable: false`,
  `INVALID_COORDINATES`
- A binary STL whose 80-byte header literally contains the text "solid" →
  still correctly detected as binary
- ASCII input with mixed case and irregular whitespace → still parses
- `TopologyBudget` and `MaxTriangles` are actually respected

## Benchmarks

```bash
go test ./internal/stlengine/... -bench=. -benchmem -run=^$
```

`BenchmarkAnalyzeBinary` runs the full pipeline (volume + dimensions +
manifold/component analysis) at 1k / 10k / 100k / 500k / 1M triangles, using
a synthetic tessellated-cube mesh sized to match each triangle count exactly
(the real ~499,832-triangle file wasn't available to benchmark against
directly — the 500k synthetic scale is sized to match it).
`BenchmarkAnalyzeBinaryNoTopology` runs the same scales with topology
analysis forced off, isolating the fast-path cost alone so you can see the
topology pass's marginal contribution. `BenchmarkAnalyzeASCII` covers the
same shapes for the ASCII reader (only through 100k by default, since ASCII
files are far larger per triangle — pass `-bench=ASCII` explicitly for
bigger scales).

Expected shape of the results (illustrative — run it on your own hardware
for real numbers, and please paste your actual output into the PR
description so the "before/after" table below can be filled in with real
measurements rather than estimates):

```
BenchmarkAnalyzeBinary/1k-8            ...      ...  ns/op    ... B/op    ... allocs/op
BenchmarkAnalyzeBinary/10k-8           ...
BenchmarkAnalyzeBinary/100k-8          ...
BenchmarkAnalyzeBinary/500k-8          ...
BenchmarkAnalyzeBinary/1M-8            ...
```

What to look for: `ns/op` should scale roughly linearly with triangle count
(no quadratic blowup), and `B/op` for `BenchmarkAnalyzeBinaryNoTopology`
should stay roughly flat per-triangle (the fast path is O(1) memory besides
I/O buffers) while `BenchmarkAnalyzeBinary`'s `B/op` grows linearly at a
modest constant (the compact edge/union-find maps) rather than the sharp
super-linear growth you'd see from a `[]int`-per-edge design.

### Before/after

I do not have access to a `go` toolchain or the actual 499,832-triangle file
in this environment, so I cannot honestly hand you a real "old build vs new
build" timing table — doing so would mean fabricating numbers, which is
exactly the kind of "faked result" this whole project is trying to avoid in
its pricing logic. Please run:

```bash
# on your current main branch:
go test ./... -run=NONE -bench=BenchmarkAnalyze -benchmem > before.txt
# after integrating this upgrade:
go test ./... -run=NONE -bench=BenchmarkAnalyze -benchmem > after.txt
benchstat before.txt after.txt   # go install golang.org/x/perf/cmd/benchstat@latest
```

and drop `before.txt`/`after.txt`/the `benchstat` diff into the PR. I've
left the benchmark harness in a state where that's a copy-paste away.
