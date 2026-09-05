// This file is meant to be added to package main in the existing
// 3d-print-slicer-api repository, replacing the STL-specific portion of
// analyzeHandler. See /docs/INTEGRATION.md for the exact, line-by-line
// instructions (what to delete from the current main.go, what to add, and
// why each change is safe).
//
// Everything here is intentionally self-contained in one new file so the
// diff against the existing repository is easy to review: nothing in
// main.go's OBJ/OFF/PLY/3MF/AMF handling, Shopify draft-order code, or CORS
// layer is touched.

package main

import (
	"bytes"
	"context"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nikhil-misal/3d-print-slicer-api/internal/config"
	"github.com/nikhil-misal/3d-print-slicer-api/internal/limiter"
	"github.com/nikhil-misal/3d-print-slicer-api/internal/stlengine"
	"github.com/nikhil-misal/3d-print-slicer-api/internal/upload"
)

// appConfig and analysisLimiter are process-wide, read-only-after-init
// values — NOT per-request mutable state, so they're safe to share across
// concurrently-served requests (see PHASE 21 / concurrency requirements).
var (
	appConfig       = config.Load()
	analysisLimiter = limiter.New(appConfig.MaxConcurrentAnalysis)
)

// wireAnalyzeRoute replaces the plain mux.HandleFunc("/analyze", analyzeHandler)
// registration in main() with the concurrency-limited version. See
// INTEGRATION.md step 1.
func wireAnalyzeRoute(mux *http.ServeMux) {
	mux.Handle("/analyze", limiter.Middleware(analysisLimiter, http.HandlerFunc(analyzeHandlerV2)))
}

// MeshDiagnostics is the additive JSON block carrying the new structured
// mesh-reliability information. It is nested under "mesh" in the response
// so existing top-level fields (volume_cm3, solid_weight_g, etc.) are
// completely unchanged for any current frontend code — this is a pure
// addition, not a breaking change.
type MeshDiagnostics struct {
	Status            string `json:"status"`
	VolumeReliable    bool   `json:"volume_reliable"`
	TopologyEvaluated bool   `json:"topology_evaluated"`
	Components        int    `json:"components"`
	CavityComponents  int    `json:"cavity_components"`
	ErrorCode         string `json:"error_code,omitempty"`
}

// DimensionsMM mirrors section 25 of the spec's example response shape.
type DimensionsMM struct {
	X float64 `json:"x_mm"`
	Y float64 `json:"y_mm"`
	Z float64 `json:"z_mm"`
}

// AnalyzeResponseV2 embeds the existing AnalyzeResponse (defined in
// main.go, unchanged) and adds the new fields alongside it, so json.Marshal
// still emits every field the current Shopify frontend already reads.
type AnalyzeResponseV2 struct {
	AnalyzeResponse
	Dimensions        DimensionsMM    `json:"dimensions"`
	Mesh              MeshDiagnostics `json:"mesh"`
	FileHashSHA256    string          `json:"file_hash_sha256,omitempty"`
	ProcessingTimeMs  int64           `json:"processing_time_ms"`
}

// analyzeHandlerV2 is the STL-aware replacement for analyzeHandler. Requests
// for OBJ/OFF/PLY/3MF/AMF are delegated, byte-for-byte, to the existing
// legacy code path (parseModel + calculateRobustMeshVolume), which is left
// exactly as-is: this change is scoped to the STL performance/reliability
// problem described in the project brief, not a rewrite of every format.
func analyzeHandlerV2(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"success": false,
			"error":   "POST method required",
		})
		return
	}

	maxBytes := appConfig.MaxUploadSizeBytes()
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	if err := r.ParseMultipartForm(maxBytes); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Invalid multipart upload: " + err.Error(),
		})
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "3D model file is required",
		})
		return
	}
	defer file.Close()

	material := strings.TrimSpace(r.FormValue("material"))
	if material == "" {
		material = "PLA"
	}
	infill := 20.0
	if raw := strings.TrimSpace(r.FormValue("infill")); raw != "" {
		if value, err := strconv.ParseFloat(raw, 64); err == nil {
			infill = value
		}
	}
	if infill < 0 {
		infill = 0
	}
	if infill > 100 {
		infill = 100
	}

	fileName := header.Filename
	if raw := strings.TrimSpace(r.FormValue("fileName")); raw != "" {
		fileName = raw
	}

	// Peek a small prefix to route the request WITHOUT reading the whole
	// upload into memory. detectFileType (defined in main.go, unchanged)
	// only ever looks at the first ~1000 bytes plus the filename
	// extension, so this is safe for every existing supported format.
	peek := make([]byte, peekWindowForRouting)
	n, _ := io.ReadFull(file, peek)
	peek = peek[:n]
	routedType := detectFileType(fileName, peek)

	if routedType != "stl" {
		// Legacy path: unchanged behavior for OBJ/OFF/PLY/3MF/AMF. We have
		// already consumed `peek` bytes from `file`, so reassemble the full
		// stream before handing it to the existing io.ReadAll-based parser.
		rest, err := io.ReadAll(file)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"error":   "Unable to read uploaded file: " + err.Error(),
			})
			return
		}
		data := append(peek, rest...)
		legacyAnalyzeAndRespond(w, routedType, data, material, infill)
		return
	}

	analyzeSTLAndRespond(w, r, peek, file, fileName, material, infill, start)
}

const peekWindowForRouting = 8192

// analyzeSTLAndRespond streams the STL upload to a temp file (hashing as it
// goes), runs the new hybrid engine against it, and writes the response.
// This is the actual fix for the original problem: no io.ReadAll of the
// full upload, no in-memory Mesh/vertex-welding map, one streaming pass.
func analyzeSTLAndRespond(w http.ResponseWriter, r *http.Request, peek []byte, file io.Reader, fileName, material string, infill float64, start time.Time) {
	saved, err := upload.StreamToTemp("", fileName, io.MultiReader(bytes.NewReader(peek), file))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   "Unable to store uploaded file for analysis",
		})
		return
	}
	defer saved.Remove() // /analyze never persists uploads; only a confirmed
	// Shopify order triggers Google Drive storage (see docs/ARCHITECTURE.md
	// "Order / Drive integration" for how saved.SHA256Hex plugs into that).

	f, err := os.Open(saved.Path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   "Unable to read stored upload",
		})
		return
	}
	defer f.Close()

	ctx, cancel := context.WithTimeout(r.Context(), appConfig.AnalysisTimeout)
	defer cancel()

	opts := stlengine.DefaultOptions()
	opts.MaxTriangles = appConfig.MaxTriangles
	opts.TopologyBudget = appConfig.TopologyBudget

	result, analyzeErr := stlengine.Analyze(ctx, f, saved.Size, opts)
	elapsed := time.Since(start).Milliseconds()

	if analyzeErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success":            false,
			"file_type":          "stl",
			"file_hash_sha256":   saved.SHA256Hex,
			"processing_time_ms": elapsed,
			"error": map[string]interface{}{
				"code":    string(result.ErrorCode),
				"message": customerSafeSTLMessage(result.ErrorCode),
			},
		})
		return
	}

	density := materialDensity(material)
	volumeCm3 := result.VolumeMM3 / 1000.0
	solidWeightG := volumeCm3 * density
	estimatedPrintWeightG := estimatePrintWeight(solidWeightG, infill)

	cavities := 0
	for _, c := range result.Components {
		if c.IsCavity {
			cavities++
		}
	}
	size := result.Dimensions.Size()

	resp := AnalyzeResponseV2{
		AnalyzeResponse: AnalyzeResponse{
			Success:               result.VolumeReliable,
			VolumeCm3:             volumeCm3,
			SolidWeightG:          solidWeightG,
			EstimatedPrintWeightG: estimatedPrintWeightG,
			TriangleCount:         int(result.TriangleCount),
			VertexCount:           int(result.TriangleCount) * 3, // no welding on the fast path; see INTEGRATION.md
			Material:              material,
			Infill:                infill,
			FileType:              "stl",
			Unit:                  "mm",
			ClosedMesh:            result.MeshStatus == stlengine.StatusClosed || result.MeshStatus == stlengine.StatusMultiShell,
			Components:            len(result.Components),
		},
		Dimensions: DimensionsMM{X: size.X, Y: size.Y, Z: size.Z},
		Mesh: MeshDiagnostics{
			Status:            string(result.MeshStatus),
			VolumeReliable:    result.VolumeReliable,
			TopologyEvaluated: result.TopologyEvaluated,
			Components:        len(result.Components),
			CavityComponents:  cavities,
			ErrorCode:         string(result.ErrorCode),
		},
		FileHashSHA256:   saved.SHA256Hex,
		ProcessingTimeMs: elapsed,
	}

	if result.VolumeReliable {
		resp.Message = "Model analyzed successfully"
	} else {
		resp.Error = customerSafeSTLMessage(result.ErrorCode)
	}

	status := http.StatusOK
	if !result.VolumeReliable {
		// Preserves the CURRENT frontend contract, where only `success:true`
		// responses carry a price-worthy volume. mesh.error_code still lets
		// an updated frontend distinguish "corrupt file" from "valid but
		// open/non-manifold" without a breaking status-code change today.
		status = http.StatusBadRequest
	}
	writeJSON(w, status, resp)
}

// customerSafeSTLMessage turns an internal ErrorCode into the kind of
// message a customer should actually see (section 50: never surface
// parser/stack internals).
func customerSafeSTLMessage(code stlengine.ErrorCode) string {
	switch code {
	case stlengine.ErrEmptyModel:
		return "No usable 3D geometry was found in this file."
	case stlengine.ErrInvalidCoordinates:
		return "This file contains invalid geometry data and could not be measured reliably."
	case stlengine.ErrOpenMesh:
		return "Your 3D model could not be measured automatically because it isn't a closed, watertight shape. Please make sure the model has no holes and try again."
	case stlengine.ErrNonManifold:
		return "Your 3D model has overlapping or inconsistent surfaces at one or more edges. Please repair the model (e.g. with a mesh-repair tool) and try again."
	case stlengine.ErrVolumeUnreliable:
		return "This model's size could not be fully verified, so we can't guarantee an accurate price yet. A team member may need to review it."
	case stlengine.ErrTruncatedSTL:
		return "This file appears to be incomplete or corrupted. Please re-export and re-upload it."
	case stlengine.ErrFileTooLarge:
		return "This model is too complex for automatic analysis at this time."
	case stlengine.ErrProcessingTimeout:
		return "Analyzing this model is taking longer than expected. Please try again."
	case stlengine.ErrInvalidFile, stlengine.ErrInvalidBinarySTL, stlengine.ErrInvalidASCIISTL:
		return "This doesn't look like a valid STL file. Please check the export and try again."
	default:
		return "Your 3D model could not be analyzed automatically. Please make sure the STL is a closed/watertight model and try again."
	}
}

// legacyAnalyzeAndRespond is the ORIGINAL analyzeHandler body for every
// non-STL format, unchanged in behavior. It exists here only so
// analyzeHandlerV2 can call it after re-assembling the peeked bytes; the
// logic itself — parseModel, calculateRobustMeshVolume, response shape —
// is copied verbatim from the current main.go and must be kept in sync if
// that code changes. See INTEGRATION.md step 3 for exactly what to move.
func legacyAnalyzeAndRespond(w http.ResponseWriter, fileType string, data []byte, material string, infill float64) {
	if len(data) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Uploaded file is empty",
		})
		return
	}

	mesh, err := parseModel(fileType, data)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success":   false,
			"file_type": fileType,
			"error":     err.Error(),
		})
		return
	}
	if len(mesh.Vertices) == 0 || len(mesh.Triangles) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "No usable geometry was found in the model",
		})
		return
	}

	volumeMm3, components, closed, err := calculateRobustMeshVolume(mesh)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success":        false,
			"file_type":      fileType,
			"triangle_count": len(mesh.Triangles),
			"vertex_count":   len(mesh.Vertices),
			"error":          err.Error(),
		})
		return
	}
	if volumeMm3 <= epsVolume || math.IsNaN(volumeMm3) || math.IsInf(volumeMm3, 0) {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success":        false,
			"file_type":      fileType,
			"triangle_count": len(mesh.Triangles),
			"vertex_count":   len(mesh.Vertices),
			"closed_mesh":    closed,
			"components":     components,
			"error":          "Unable to calculate a valid solid volume. The model may contain open edges, non-manifold geometry, self-intersections, or invalid mesh orientation.",
		})
		return
	}

	volumeCm3 := volumeMm3 / 1000.0
	density := materialDensity(material)
	solidWeightG := volumeCm3 * density
	estimatedPrintWeightG := estimatePrintWeight(solidWeightG, infill)

	writeJSON(w, http.StatusOK, AnalyzeResponse{
		Success:               true,
		VolumeCm3:             volumeCm3,
		SolidWeightG:          solidWeightG,
		EstimatedPrintWeightG: estimatedPrintWeightG,
		TriangleCount:         len(mesh.Triangles),
		VertexCount:           len(mesh.Vertices),
		Material:              material,
		Infill:                infill,
		FileType:              fileType,
		Unit:                  mesh.Unit,
		ClosedMesh:            closed,
		Components:            components,
		Message:               "Model analyzed successfully",
	})
}
