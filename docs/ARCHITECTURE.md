# Architecture: STL Analysis Engine v2

## 1. What was actually happening today (audit findings)

I inspected the live repository at `github.com/nikhil-misal/3d-print-slicer-api`
(module `github.com/nikhil-misal/3d-print-slicer-api`, Go 1.24). I was able to
retrieve `main.go` lines 1–1000 of 2182 and `go.mod` directly; GitHub's raw-file
endpoint returned a robots-disallowed error inside this sandbox for the
remainder, and this environment has no outbound network access for `git
clone`, so I could not read `calculateRobustMeshVolume`, `materialDensity`,
`estimatePrintWeight`, the 3MF/AMF parsers, or the Shopify/Google Drive code
directly. Everything below about those specific functions is inferred only
from their **call sites**, which I did see in full — I'm calling them exactly
as the existing code does, not redefining them. Section 6 of `INTEGRATION.md`
flags the one place you should sanity-check this yourself before merging.

What the visible 1000 lines confirmed, precisely:

- **`analyzeHandler` calls `io.ReadAll(file)`** on the entire multipart upload
  before doing anything else (`main.go`, in `analyzeHandler`). For a 500k+
  triangle binary STL (~25MB) that's a 25MB buffer just to get started — not
  catastrophic by itself, but it's the first of several full-file copies.
- **Every format — including STL — goes through `meshBuilder`**, which keeps
  a `map[vertexKey]int` (a `map[struct{X,Y,Z int64}]int`) *and* a parallel
  `[]Point` slice, deduplicating every vertex via a hashed lookup
  (`newMeshBuilder`, `addVertex`, `makeVertexKey`). This is necessary for
  OBJ/OFF/PLY, which reference vertices by index and *need* a vertex table.
  It is **not** necessary for STL, which repeats all 3 vertices per triangle
  in the file already — welding them into a shared table is pure overhead
  for the volume calculation, and it's where a big chunk of the memory for
  large STL files goes today.
- **`isBinarySTL` / binary vs ASCII detection already uses the size-formula
  check** (`84 + count*50 == len(data)`), which is good and I kept that
  approach — I did not have to fix a "starts with solid" bug in the *binary
  detector* itself, contrary to what I assumed before reading the code. What
  I *did* still need to fix: it currently requires the **whole file already
  in RAM** (`data []byte`) to run that check, because `analyzeHandler` reads
  everything up front. The new engine runs the same formula off an 8KB peek
  of a streamed file, with no behavior change to the detection logic itself.
- **ASCII parsing uses `bufio.Scanner` line-by-line** with a manageable
  64KiB→1MiB buffer already configured (`parseASCIISTL`) — better than I
  expected. The new engine still improves on this by tokenizing on
  whitespace instead of lines (see §4), which is strictly more tolerant of
  unusual formatting, and by never materializing the full file first.
- **`calculateRobustMeshVolume`, referenced from `analyzeHandler` as
  `(volumeMm3, components, closed, err)`, is the one function I could not
  read.** You (the repo owner) described its internals directly in the
  brief — `edgeMap`, `edgeUse{Triangles []int; Dirs []int}`, `adjacency`,
  `componentInfo`, `pointInsideComponent`, `rayIntersectsTriangle` — and
  those names are exactly the shape of algorithm that gets slow on a
  500k-triangle mesh: a `[]int` per edge means a separate heap allocation
  per edge (~750k allocations for that file), and `pointInsideComponent` /
  `rayIntersectsTriangle` describe ray-casting against triangles for
  containment, which is O(N) *per query* — fine once, ruinous if called
  per-component-pair without a bounding-box prefilter first. That matches
  the symptom you described (works for simple files, chokes on complex
  ones) far better than the parsing code does, which is why the new engine
  replaces this function's entire *strategy*, not just its constants.

## 2. The new pipeline

```
UPLOAD (multipart)
   │  http.MaxBytesReader (configurable cap)
   ▼
STREAM TO TEMP FILE  ──────────────►  SHA-256 hash (same pass, io.MultiWriter)
   │  (internal/upload; io.Copy, 32KiB chunks — never io.ReadAll)
   ▼
PEEK ~8KB  →  FORMAT DETECTION (binary size-formula OR ascii heuristic;
              overflow-safe; ignores header text like "solid")
   ▼
STREAMING TRIANGLE READER
   binary: fixed [50]byte buffer reused every triangle, io.ReadFull
   ascii:  whitespace tokenizer over bufio.Reader, no line-length limit,
           no full-file load
   ▼
SINGLE PASS, PER TRIANGLE:
   • bounding box (min/max, O(1))
   • signed tetrahedron volume, Neumaier-compensated sum,
     origin = first vertex seen (O(1) memory, no second pass,
     numerically stable even 10^6 mm from the world origin)
   • cheap checks: finite coords / zero-area / degenerate triangle
   • IF within TopologyBudget: fold into a compact union-find
     (component membership) + compact edge-parity map
     (open/non-manifold detection) — same pass, no second read
   ▼
IF multiple shells: O(C²) bounding-box nesting check (C = shell count,
   NOT triangle count) → cavities subtracted, not added
   ▼
RESULT: volume, dimensions, mesh_status, volume_reliable, components,
        diagnostics — original file untouched, never rewritten
```

Nothing here builds a full `Mesh{Vertices, Triangles}` for STL. There is no
vertex-welding map, no adjacency graph, and no `[]int`-per-edge structure at
any point.

## 3. Why one pass is enough for dimensions *and* a stable volume

The classic reason people reach for two passes is: "I need the model's
centroid/local origin for numerical stability, but I don't know it until
I've seen every vertex." That's true if you insist on using the centroid.
It's not necessary: **any** fixed reference point works for the signed
tetrahedron volume formula, because translating the whole coordinate system
by a constant doesn't change the enclosed volume of a closed surface — the
constant cancels out in the sum. So the engine uses the **first vertex it
reads** as the reference origin. That point is *on* the model by
construction, so every subsequent `(vertex - origin)` is a small, well-scaled
number — exactly the numerical benefit a local origin is supposed to give —
with zero extra passes and zero extra memory. `TestCubeTranslatedToLargeCoordinates`
in `stlengine_test.go` verifies this directly: a 10mm cube translated
1,000,000mm from the world origin still measures ~1000mm³.

## 4. Why the ASCII reader tokenizes instead of scanning lines

`bufio.Scanner` has a default 64KiB **per-token** limit, which for
line-oriented scanning means per-line. The existing parser already raises
that to 1MiB, which handles ordinary files fine. The new reader goes one
step further and doesn't scan lines at all — it reads a byte at a time from
a `bufio.Reader` (so still buffered, still fast) and splits on any
whitespace run. This means arbitrary spacing, tabs, or one facet crammed onto
one very long line all parse the same way, with no line-length concept to
violate. See `TestASCIIWithUnusualWhitespaceAndCase`.

## 5. Why topology/manifold checking costs O(edges) memory, and why that's OK

Determining whether a mesh is closed and consistently oriented is
fundamentally a per-edge accounting problem: you cannot know an edge is
shared by exactly two triangles, in opposite directions, without counting.
There is no O(1)-memory algorithm for this — any implementation that claims
otherwise is either not actually checking watertightness or is checking a
weaker, unsound proxy for it.

What the engine controls is the **cost per edge**:

| | previous design (as described) | this engine |
|---|---|---|
| Edge value | `edgeUse{Triangles []int, Dirs []int}` — a struct holding two growing slices, one heap allocation each, per edge | `edgeState{Count int32, Dir int32}` — 8 bytes, no allocation, stored by value in the map |
| Vertex identity | Full `Point{X,Y,Z float64}` table + `map[vertexKey]int`, kept for the whole mesh | Quantized `vKey{X,Y,Z int64}` used only as a map key; no coordinate table is retained |
| Component detection | (unseen, but implied by `componentInfo`/`pointInsideComponent`) apparently ray-casting per component | Weighted union-find over vertex identity, O(N·α(N)) |
| Nesting/cavities | (unseen) apparently full point-in-mesh per pair | Bounding-box containment, O(C²) in **shell count**, not triangle count |

This is still O(N) memory for the topology pass — that's inherent to the
problem, not a design flaw — but it's a small, fixed multiple of N instead of
N variable-length slices, and it's built during the *same* pass that already
computes volume, not a separate pass. `Options.TopologyBudget` (default 6M
triangles) is the safety valve: above that count, the engine still returns
an exact volume and dimensions (that part really is O(1) memory), but
reports `mesh_status: "suspicious"` and `volume_reliable: false` rather than
building an unbounded map. This is the direct implementation of "if
validation requires O(N) memory, explain why" from the brief.

## 6. Cavity / multi-shell handling

Components are tracked via union-find over vertex identity, merged
incrementally in the same triangle pass (see `topology.go`'s doc comment for
exactly how per-component volume/bbox aggregates survive union operations
without a second pass). Once every shell's own bounding box and unsigned
volume are known, `nesting.go` classifies each shell by **nesting parity**:
a shell nested inside an odd number of larger shells is a cavity (its
volume is subtracted); nested inside an even number (including zero) it's
solid material (added). This handles the common single-cavity case and the
rarer double-nested case (solid island inside a hole inside a solid), using
only O(C²) bounding-box comparisons where C is the number of shells — for
ordinary uploads C=1 and this is skipped entirely.

### Known simplification

Nesting is decided by bounding-box containment, not exact point-in-mesh
testing. This is correct for the overwhelmingly common case (a hollow
shape's cavity is a smaller shell that doesn't touch the outer wall) and
is flagged (`mesh_status: "suspicious"`, `volume_reliable: false`) rather
than guessed at when two shells' bounding boxes overlap without one
cleanly containing the other. A true point-in-mesh fallback (ray-casting
against a spatial grid built from just the ambiguous shells, re-read from
the temp file) is a well-defined next step if this turns out to matter for
real customer files, but it was left out of this pass to keep the shipped
code small enough to review and test with confidence in one sitting —
better to ship a documented, honest limitation than a subtle bug in
untested geometry code.

## 7. What did NOT change

- OBJ, OFF, PLY, 3MF, and AMF parsing and volume calculation: **untouched**,
  still going through the existing `parseModel` / `calculateRobustMeshVolume`
  path. This upgrade is scoped to the STL problem described in the brief.
- The Shopify draft-order endpoint, CORS middleware, and health/app handlers:
  untouched.
- Material densities and the existing infill→weight approximation: untouched
  — `materialDensity` and `estimatePrintWeight` are called exactly as before.
- The original uploaded bytes: never modified. The engine only ever reads.

## 8. Complexity summary

| Stage | Time | Extra memory |
|---|---|---|
| Format detection | O(1) (8KB peek) | O(1) |
| Volume + bbox + cheap checks | O(N) | O(1) |
| Topology (manifold + components), within budget | O(N) | O(N) (compact, ~24–40 bytes/edge incl. map overhead) |
| Shell nesting classification | O(C²), C = shell count | O(C) |

N = triangle count. There is no O(N²) step anywhere in this pipeline for any
input shape.
