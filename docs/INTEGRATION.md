# Integration Guide

This upgrade ships as new files that sit alongside your existing `main.go`
rather than a rewrite of it, so the diff is easy to review. Apply these
steps in order.

## 0. Copy files into the repository

```
your-repo/
├── main.go                          (existing — small edits below)
├── analyze_handler.go                (NEW — from this delivery)
├── go.mod                            (unchanged)
├── internal/
│   ├── stlengine/                    (NEW — the whole package)
│   ├── upload/upload.go              (NEW)
│   ├── config/config.go              (NEW)
│   └── limiter/limiter.go            (NEW)
```

Everything under `internal/` and `analyze_handler.go` compiles standalone —
no new dependencies, no changes to `go.mod` are required (all imports are
Go standard library plus your own module path).

## 1. Wire the new route in `main()`

In your current `main()`:

```go
mux := http.NewServeMux()
mux.HandleFunc("/", healthHandler)
mux.HandleFunc("/app", appHandler)
mux.HandleFunc("/analyze", analyzeHandler)
mux.HandleFunc("/create-draft-order", createDraftOrderHandler)
```

Change the `/analyze` line to:

```go
mux := http.NewServeMux()
mux.HandleFunc("/", healthHandler)
mux.HandleFunc("/app", appHandler)
wireAnalyzeRoute(mux) // was: mux.HandleFunc("/analyze", analyzeHandler)
mux.HandleFunc("/create-draft-order", createDraftOrderHandler)
```

`wireAnalyzeRoute` (defined in `analyze_handler.go`) registers
`analyzeHandlerV2` wrapped in the concurrency limiter from
`internal/limiter`.

## 2. Remove the old `analyzeHandler` function

Delete the entire `analyzeHandler` function from `main.go` (the one starting
`func analyzeHandler(w http.ResponseWriter, r *http.Request) {` and its
`/* ANALYZE */` comment block above it). Its logic has been split into
`analyzeHandlerV2` (routing + STL path) and `legacyAnalyzeAndRespond`
(unchanged OBJ/OFF/PLY/3MF/AMF path), both in `analyze_handler.go`.

Everything else in `main.go` — `detectFileType`, `parseModel`,
`meshBuilder`/`newMeshBuilder`/`addVertex`/`addTriangle`, `parseSTL`,
`isBinarySTL`, `looksLikeASCIISTL`, `parseBinarySTL`, `parseASCIISTL`,
`parseOBJ`, `parseOFF`, `parsePLY*`, `parse3MF`, `parseAMF`,
`calculateRobustMeshVolume`, `materialDensity`, `estimatePrintWeight`,
`createDraftOrderHandler`, the Shopify/Drive code, CORS, health/app
handlers — is **untouched**. `analyze_handler.go` calls several of these
(`detectFileType`, `parseModel`, `calculateRobustMeshVolume`,
`materialDensity`, `estimatePrintWeight`, `writeJSON`, `AnalyzeResponse`,
`epsVolume`) exactly as they exist today.

> Note: `parseSTL`, `isBinarySTL`, `looksLikeASCIISTL`, `parseBinarySTL`, and
> `parseASCIISTL` become unreachable dead code once STL requests are routed
> to `analyzeSTLAndRespond` instead of `parseModel("stl", ...)`. Go does not
> error on unused top-level functions, so leaving them in place is safe if
> you want a smaller diff; delete them later once you've validated the new
> path in production, or keep them temporarily as a manual fallback (see
> §5 rollback).

## 3. Retire the old `maxUploadSize` constant (optional but recommended)

`analyzeHandlerV2` reads its limit from `internal/config.Load()`
(`MAX_UPLOAD_SIZE_MB` env var, default 50 — the same default you have
today). The old `maxUploadSize = 50 << 20` constant in `main.go` is no
longer read by anything in the new path; leave it if other code still
references it, otherwise delete it.

## 4. One thing to verify yourself before deploying

I was able to read `main.go` lines 1–1000 of 2182 directly from GitHub in
this session (imports, all type definitions, `analyzeHandler`,
`detectFileType`, `parseModel`, the vertex-welding builder, and the
STL/OBJ/OFF/PLY parsers). GitHub's raw-file endpoint returned a
robots-disallowed error for the rest of the file inside this sandbox, and
the sandbox itself has no outbound network access to `git clone` the repo
directly — so I could not read `calculateRobustMeshVolume`,
`materialDensity`, or `estimatePrintWeight` bodies, only their call sites
(which is all `analyze_handler.go` needs, since it calls them, not
redefines them).

Please double check, once you paste these files in:

1. **`materialDensity(material string) float64`** and
   **`estimatePrintWeight(solidWeightG, infillPercent float64) float64`** —
   confirm these signatures match what's in your `main.go` today (they
   should, based on the call site I read at line ~370 of the original
   `analyzeHandler`).
2. **`calculateRobustMeshVolume(mesh Mesh) (volumeMm3 float64, components int, closed bool, err error)`**
   — confirmed from the call site; `legacyAnalyzeAndRespond` calls it with
   this exact signature for non-STL formats. If the real signature differs,
   the compiler will tell you immediately at that one call site — nothing
   else in this delivery depends on it.
3. Run `go build ./...` after copying the files in. I was not able to run
   the Go toolchain in this sandbox (no `go` binary available, no network to
   install one), so this delivery has been written and carefully
   hand-reviewed for correctness but **has not been compiled**. Please treat
   the first `go build` / `go vet` / `go test ./...` run as part of
   integration, not as a formality — see §6.

## 5. Rollback

If anything looks wrong after deploying, reverting is a one-line change:
swap `wireAnalyzeRoute(mux)` back to
`mux.HandleFunc("/analyze", analyzeHandler)` (as long as you haven't
deleted the old function yet) and redeploy. No data migration, no schema,
nothing else to undo — the new code path doesn't touch anything the old
path didn't already own.

## 6. Before merging: build, vet, test, benchmark

```bash
go build ./...
go vet ./...
go test ./...
go test ./internal/stlengine/... -bench=. -benchmem
```

See `README.md` for expected benchmark output shapes and what to look for.
