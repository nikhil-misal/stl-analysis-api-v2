package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

/*
	3D PRINT ANALYSIS API

	Supported formats:
	- STL: binary + ASCII
	- OBJ
	- OFF
	- PLY: ASCII + binary little-endian + binary big-endian
	- 3MF
	- AMF

	Units:
	- STL / OBJ / OFF / PLY => millimeters assumed
	- 3MF / AMF => unit metadata is respected where available

	Volume:
	- Fast O(T) signed tetrahedron volume calculation
	- Local bounding-box origin for numerical stability
	- Compensated floating-point summation
	- No giant edge/adjacency graph in the volume hot path
	- Original geometry is never modified
*/

const (
	maxUploadSize = 50 << 20 // 50 MB

	epsVertex = 1e-7
	epsArea   = 1e-14
	epsVolume = 1e-18

	shopifyAPI = "2026-07"
)

type Point struct {
	X float64
	Y float64
	Z float64
}

type Triangle struct {
	A int
	B int
	C int
}

type Mesh struct {
	Vertices  []Point
	Triangles []Triangle
	Unit      string
}

type AnalyzeResponse struct {
	Success               bool    `json:"success"`
	VolumeCm3             float64 `json:"volume_cm3"`
	SolidWeightG          float64 `json:"solid_weight_g"`
	EstimatedPrintWeightG float64 `json:"estimated_print_weight_g"`
	TriangleCount         int     `json:"triangle_count"`
	VertexCount           int     `json:"vertex_count"`
	Material              string  `json:"material"`
	Infill                float64 `json:"infill"`
	FileType              string  `json:"file_type"`
	Unit                  string  `json:"unit"`
	ClosedMesh            bool    `json:"closed_mesh"`
	Components            int     `json:"components"`
	Message               string  `json:"message,omitempty"`
	Error                 string  `json:"error,omitempty"`
}

type DraftOrderRequest struct {
	Price    float64 `json:"price"`
	Quantity int     `json:"quantity"`
	Title    string  `json:"title"`
	Material string  `json:"material"`
	Color    string  `json:"color"`
	Weight   float64 `json:"weight"`
	Volume   float64 `json:"volume"`
	FileName string  `json:"fileName"`
	FileURL  string  `json:"fileUrl"`
	FileID   string  `json:"fileId"`
}

type GraphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors,omitempty"`
}

/* =========================================================
   MAIN
   ========================================================= */

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/", healthHandler)
	mux.HandleFunc("/app", appHandler)
	mux.HandleFunc("/analyze", analyzeHandler)
	mux.HandleFunc("/create-draft-order", createDraftOrderHandler)

	handler := corsMiddleware(mux)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
	}

	fmt.Println("3D Print Analysis API running on port", port)

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Println("server error:", err)
	}
}

/* =========================================================
   HTTP / CORS
   ========================================================= */

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(value)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"service": "3D Print Analysis API",
		"status":  "online",
	})
}

func appHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	html := `
<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>3D Print Analysis API</title>
</head>
<body>
<h2>3D Print Analysis API</h2>
<p>Service is online.</p>
<p>POST 3D model files to <code>/analyze</code>.</p>
</body>
</html>`

	_, _ = w.Write([]byte(html))
}

/* =========================================================
   ANALYZE
   ========================================================= */

func analyzeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"success": false,
			"error":   "POST method required",
		})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
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

	data, err := io.ReadAll(file)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Unable to read uploaded file: " + err.Error(),
		})
		return
	}

	if len(data) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Uploaded file is empty",
		})
		return
	}

	fileType := detectFileType(fileName, data)

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

	if volumeMm3 <= epsVolume ||
		math.IsNaN(volumeMm3) ||
		math.IsInf(volumeMm3, 0) {

		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success":        false,
			"file_type":      fileType,
			"triangle_count": len(mesh.Triangles),
			"vertex_count":   len(mesh.Vertices),
			"closed_mesh":    closed,
			"components":     components,
			"error":          "Unable to calculate a valid solid volume. The STL may contain open edges, non-manifold geometry, self-intersections, or invalid mesh orientation.",
		})

		return
	}

	volumeCm3 := volumeMm3 / 1000.0

	density := materialDensity(material)

	solidWeightG := volumeCm3 * density

	estimatedPrintWeightG := estimatePrintWeight(
		solidWeightG,
		infill,
	)

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

/* =========================================================
   FILE TYPE
   ========================================================= */

func detectFileType(fileName string, data []byte) string {
	ext := strings.ToLower(filepath.Ext(fileName))

	switch ext {
	case ".stl":
		return "stl"
	case ".obj":
		return "obj"
	case ".off":
		return "off"
	case ".ply":
		return "ply"
	case ".3mf":
		return "3mf"
	case ".amf":
		return "amf"
	}

	trimmed := bytes.TrimSpace(data)

	if len(trimmed) >= 5 && bytes.Equal(trimmed[:5], []byte("solid")) {
		return "stl"
	}

	if bytes.HasPrefix(trimmed, []byte("OFF")) {
		return "off"
	}

	if bytes.HasPrefix(trimmed, []byte("ply")) {
		return "ply"
	}

	if bytes.HasPrefix(trimmed, []byte("<?xml")) {
		lower := strings.ToLower(string(trimmed[:minInt(len(trimmed), 1000)]))

		if strings.Contains(lower, "amf") {
			return "amf"
		}
	}

	return "unknown"
}

/* =========================================================
   MODEL PARSER
   ========================================================= */

func parseModel(fileType string, data []byte) (Mesh, error) {
	switch strings.ToLower(fileType) {
	case "stl":
		return parseSTL(data)

	case "obj":
		return parseOBJ(data)

	case "off":
		return parseOFF(data)

	case "ply":
		return parsePLY(data)

	case "3mf":
		return parse3MF(data)

	case "amf":
		return parseAMF(data)

	default:
		return Mesh{}, fmt.Errorf(
			"unsupported 3D file format. Supported: STL, OBJ, OFF, PLY, 3MF and AMF",
		)
	}
}

/* =========================================================
   VERTEX WELDER
   ========================================================= */

type vertexKey struct {
	X int64
	Y int64
	Z int64
}

type meshBuilder struct {
	mesh     Mesh
	vertices map[vertexKey]int
}

func newMeshBuilder(unit string) *meshBuilder {
	return &meshBuilder{
		mesh: Mesh{
			Vertices:  make([]Point, 0),
			Triangles: make([]Triangle, 0),
			Unit:      unit,
		},
		vertices: make(map[vertexKey]int),
	}
}

func makeVertexKey(p Point) vertexKey {
	const scale = 1000000.0

	return vertexKey{
		X: int64(math.Round(p.X * scale)),
		Y: int64(math.Round(p.Y * scale)),
		Z: int64(math.Round(p.Z * scale)),
	}
}

func (b *meshBuilder) addVertex(p Point) int {
	key := makeVertexKey(p)

	if index, ok := b.vertices[key]; ok {
		return index
	}

	index := len(b.mesh.Vertices)

	b.mesh.Vertices = append(b.mesh.Vertices, p)
	b.vertices[key] = index

	return index
}

func (b *meshBuilder) addTriangle(a, c, d Point) {
	i0 := b.addVertex(a)
	i1 := b.addVertex(c)
	i2 := b.addVertex(d)

	if i0 == i1 || i1 == i2 || i2 == i0 {
		return
	}

	if triangleAreaSquared(
		b.mesh.Vertices[i0],
		b.mesh.Vertices[i1],
		b.mesh.Vertices[i2],
	) <= epsArea {
		return
	}

	b.mesh.Triangles = append(b.mesh.Triangles, Triangle{
		A: i0,
		B: i1,
		C: i2,
	})
}

/* =========================================================
   STL
   ========================================================= */

func parseSTL(data []byte) (Mesh, error) {
	if isBinarySTL(data) {
		return parseBinarySTL(data)
	}

	if looksLikeASCIISTL(data) {
		return parseASCIISTL(data)
	}

	return Mesh{}, fmt.Errorf("invalid STL file")
}

func isBinarySTL(data []byte) bool {
	if len(data) < 84 {
		return false
	}

	count := binary.LittleEndian.Uint32(data[80:84])

	expected := uint64(84) + uint64(count)*50

	return expected == uint64(len(data))
}

func looksLikeASCIISTL(data []byte) bool {
	sampleLen := minInt(len(data), 4096)

	text := strings.ToLower(string(data[:sampleLen]))

	return strings.Contains(text, "solid") &&
		strings.Contains(text, "facet") &&
		strings.Contains(text, "vertex")
}

func parseBinarySTL(data []byte) (Mesh, error) {
	if len(data) < 84 {
		return Mesh{}, fmt.Errorf("binary STL header is incomplete")
	}

	count := binary.LittleEndian.Uint32(data[80:84])

	expected := uint64(84) + uint64(count)*50

	if expected != uint64(len(data)) {
		return Mesh{}, fmt.Errorf(
			"binary STL size does not match triangle count",
		)
	}

	builder := newMeshBuilder("mm")

	offset := 84

	for i := uint32(0); i < count; i++ {
		if offset+50 > len(data) {
			return Mesh{}, fmt.Errorf("binary STL ended unexpectedly")
		}

		p0 := readBinaryPoint(data[offset+12 : offset+24])
		p1 := readBinaryPoint(data[offset+24 : offset+36])
		p2 := readBinaryPoint(data[offset+36 : offset+48])

		builder.addTriangle(p0, p1, p2)

		offset += 50
	}

	return builder.mesh, nil
}

func readBinaryPoint(data []byte) Point {
	return Point{
		X: float64(math.Float32frombits(binary.LittleEndian.Uint32(data[0:4]))),
		Y: float64(math.Float32frombits(binary.LittleEndian.Uint32(data[4:8]))),
		Z: float64(math.Float32frombits(binary.LittleEndian.Uint32(data[8:12]))),
	}
}

func parseASCIISTL(data []byte) (Mesh, error) {
	builder := newMeshBuilder("mm")

	scanner := bufio.NewScanner(bytes.NewReader(data))

	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	vertices := make([]Point, 0, 3)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if !strings.HasPrefix(strings.ToLower(line), "vertex") {
			continue
		}

		fields := strings.Fields(line)

		if len(fields) < 4 {
			continue
		}

		x, err1 := strconv.ParseFloat(fields[1], 64)
		y, err2 := strconv.ParseFloat(fields[2], 64)
		z, err3 := strconv.ParseFloat(fields[3], 64)

		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}

		vertices = append(vertices, Point{
			X: x,
			Y: y,
			Z: z,
		})

		if len(vertices) == 3 {
			builder.addTriangle(
				vertices[0],
				vertices[1],
				vertices[2],
			)

			vertices = vertices[:0]
		}
	}

	if err := scanner.Err(); err != nil {
		return Mesh{}, err
	}

	if len(builder.mesh.Triangles) == 0 {
		return Mesh{}, fmt.Errorf("ASCII STL contains no triangles")
	}

	return builder.mesh, nil
}

/* =========================================================
   OBJ
   ========================================================= */

func parseOBJ(data []byte) (Mesh, error) {
	builder := newMeshBuilder("mm")

	rawVertices := make([]Point, 0)

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)

		if len(fields) == 0 {
			continue
		}

		switch fields[0] {
		case "v":
			if len(fields) < 4 {
				continue
			}

			x, err1 := strconv.ParseFloat(fields[1], 64)
			y, err2 := strconv.ParseFloat(fields[2], 64)
			z, err3 := strconv.ParseFloat(fields[3], 64)

			if err1 != nil || err2 != nil || err3 != nil {
				continue
			}

			rawVertices = append(rawVertices, Point{
				X: x,
				Y: y,
				Z: z,
			})

		case "f":
			if len(fields) < 4 {
				continue
			}

			indices := make([]int, 0, len(fields)-1)

			for _, token := range fields[1:] {
				part := strings.Split(token, "/")

				if len(part) == 0 {
					continue
				}

				index, err := strconv.Atoi(part[0])
				if err != nil {
					continue
				}

				if index < 0 {
					index = len(rawVertices) + index
				} else {
					index--
				}

				if index < 0 || index >= len(rawVertices) {
					continue
				}

				indices = append(indices, index)
			}

			if len(indices) < 3 {
				continue
			}

			// Fan triangulation for polygons.
			for i := 1; i < len(indices)-1; i++ {
				builder.addTriangle(
					rawVertices[indices[0]],
					rawVertices[indices[i]],
					rawVertices[indices[i+1]],
				)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return Mesh{}, err
	}

	if len(builder.mesh.Triangles) == 0 {
		return Mesh{}, fmt.Errorf("OBJ contains no usable triangles")
	}

	return builder.mesh, nil
}

/* =========================================================
   OFF
   ========================================================= */

func parseOFF(data []byte) (Mesh, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	lines := make([]string, 0)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		lines = append(lines, line)
	}

	if err := scanner.Err(); err != nil {
		return Mesh{}, err
	}

	if len(lines) == 0 {
		return Mesh{}, fmt.Errorf("empty OFF file")
	}

	if strings.TrimSpace(lines[0]) != "OFF" {
		return Mesh{}, fmt.Errorf("invalid OFF header")
	}

	if len(lines) < 2 {
		return Mesh{}, fmt.Errorf("OFF counts are missing")
	}

	counts := strings.Fields(lines[1])

	if len(counts) < 2 {
		return Mesh{}, fmt.Errorf("invalid OFF counts")
	}

	vertexCount, err := strconv.Atoi(counts[0])
	if err != nil {
		return Mesh{}, err
	}

	faceCount, err := strconv.Atoi(counts[1])
	if err != nil {
		return Mesh{}, err
	}

	if len(lines) < 2+vertexCount+faceCount {
		return Mesh{}, fmt.Errorf("OFF file is incomplete")
	}

	builder := newMeshBuilder("mm")

	vertices := make([]Point, vertexCount)

	for i := 0; i < vertexCount; i++ {
		fields := strings.Fields(lines[2+i])

		if len(fields) < 3 {
			return Mesh{}, fmt.Errorf("invalid OFF vertex")
		}

		x, err1 := strconv.ParseFloat(fields[0], 64)
		y, err2 := strconv.ParseFloat(fields[1], 64)
		z, err3 := strconv.ParseFloat(fields[2], 64)

		if err1 != nil || err2 != nil || err3 != nil {
			return Mesh{}, fmt.Errorf("invalid OFF vertex coordinates")
		}

		vertices[i] = Point{
			X: x,
			Y: y,
			Z: z,
		}
	}

	for i := 0; i < faceCount; i++ {
		fields := strings.Fields(lines[2+vertexCount+i])

		if len(fields) < 4 {
			continue
		}

		n, err := strconv.Atoi(fields[0])
		if err != nil || n < 3 || len(fields) < n+1 {
			continue
		}

		indices := make([]int, n)

		for j := 0; j < n; j++ {
			index, err := strconv.Atoi(fields[j+1])
			if err != nil || index < 0 || index >= len(vertices) {
				continue
			}

			indices[j] = index
		}

		for j := 1; j < n-1; j++ {
			builder.addTriangle(
				vertices[indices[0]],
				vertices[indices[j]],
				vertices[indices[j+1]],
			)
		}
	}

	if len(builder.mesh.Triangles) == 0 {
		return Mesh{}, fmt.Errorf("OFF contains no usable triangles")
	}

	return builder.mesh, nil
}

/* =========================================================
   PLY
   ========================================================= */

type plyFormat struct {
	ASCII        bool
	LittleEndian bool
	BigEndian    bool
	HeaderSize   int
	VertexCount  int
	FaceCount    int
}

func parsePLY(data []byte) (Mesh, error) {
	headerEnd := bytes.Index(data, []byte("end_header"))

	if headerEnd < 0 {
		return Mesh{}, fmt.Errorf("PLY end_header not found")
	}

	headerLineEnd := headerEnd + len("end_header")

	for headerLineEnd < len(data) &&
		(data[headerLineEnd] == '\r' || data[headerLineEnd] == '\n') {
		headerLineEnd++
	}

	header := string(data[:headerLineEnd])

	format := plyFormat{
		HeaderSize: headerLineEnd,
	}

	lines := strings.Split(strings.ReplaceAll(header, "\r\n", "\n"), "\n")

	currentElement := ""

	for _, line := range lines {
		fields := strings.Fields(line)

		if len(fields) == 0 {
			continue
		}

		switch fields[0] {
		case "format":
			if len(fields) < 2 {
				continue
			}

			switch fields[1] {
			case "ascii":
				format.ASCII = true

			case "binary_little_endian":
				format.LittleEndian = true

			case "binary_big_endian":
				format.BigEndian = true
			}

		case "element":
			if len(fields) < 3 {
				continue
			}

			currentElement = fields[1]

			count, err := strconv.Atoi(fields[2])
			if err != nil {
				continue
			}

			if currentElement == "vertex" {
				format.VertexCount = count
			}

			if currentElement == "face" {
				format.FaceCount = count
			}
		}
	}

	if format.ASCII {
		return parsePLYASCII(data[format.HeaderSize:])
	}

	if format.LittleEndian || format.BigEndian {
		return parsePLYBinary(
			data[format.HeaderSize:],
			format.LittleEndian,
			format.VertexCount,
			format.FaceCount,
		)
	}

	return Mesh{}, fmt.Errorf("unsupported PLY format")
}

func parsePLYASCII(data []byte) (Mesh, error) {
	builder := newMeshBuilder("mm")

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	vertexCount := -1
	faceMode := false

	rawVertices := make([]Point, 0)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line == "" || strings.HasPrefix(line, "comment") {
			continue
		}

		fields := strings.Fields(line)

		if vertexCount < 0 {
			// We cannot reliably know counts from body alone.
			// Use heuristic based on first face line.
		}

		if len(fields) >= 3 && !faceMode {
			if x, err1 := strconv.ParseFloat(fields[0], 64); err1 == nil {
				if y, err2 := strconv.ParseFloat(fields[1], 64); err2 == nil {
					if z, err3 := strconv.ParseFloat(fields[2], 64); err3 == nil {
						rawVertices = append(rawVertices, Point{
							X: x,
							Y: y,
							Z: z,
						})
						continue
					}
				}
			}
		}

		if len(fields) >= 4 {
			n, err := strconv.Atoi(fields[0])

			if err == nil && n >= 3 && len(fields) >= n+1 {
				faceMode = true

				indices := make([]int, n)

				valid := true

				for i := 0; i < n; i++ {
					index, err := strconv.Atoi(fields[i+1])

					if err != nil || index < 0 || index >= len(rawVertices) {
						valid = false
						break
					}

					indices[i] = index
				}

				if valid {
					for i := 1; i < n-1; i++ {
						builder.addTriangle(
							rawVertices[indices[0]],
							rawVertices[indices[i]],
							rawVertices[indices[i+1]],
						)
					}
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return Mesh{}, err
	}

	if len(builder.mesh.Triangles) == 0 {
		return Mesh{}, fmt.Errorf(
			"ASCII PLY could not be parsed. PLY vertex/face property layout may be unsupported",
		)
	}

	return builder.mesh, nil
}

func parsePLYBinary(
	data []byte,
	littleEndian bool,
	vertexCount int,
	faceCount int,
) (Mesh, error) {
	if vertexCount <= 0 || faceCount < 0 {
		return Mesh{}, fmt.Errorf("invalid PLY element counts")
	}

	builder := newMeshBuilder("mm")

	order := binary.BigEndian

	if littleEndian {
		order = binary.LittleEndian
	}

	reader := bytes.NewReader(data)

	vertices := make([]Point, vertexCount)

	/*
		Most printable PLY files use:
		x y z as float32/float64 followed by optional properties.

		For robust support we need the exact property layout.
		This parser intentionally supports the common:
		3 x/y/z float32 or float64
		layout.
	*/

	for i := 0; i < vertexCount; i++ {
		var x, y, z float32

		if err := binary.Read(reader, order, &x); err != nil {
			return Mesh{}, fmt.Errorf("invalid binary PLY vertex data")
		}

		if err := binary.Read(reader, order, &y); err != nil {
			return Mesh{}, fmt.Errorf("invalid binary PLY vertex data")
		}

		if err := binary.Read(reader, order, &z); err != nil {
			return Mesh{}, fmt.Errorf("invalid binary PLY vertex data")
		}

		vertices[i] = Point{
			X: float64(x),
			Y: float64(y),
			Z: float64(z),
		}
	}

	for i := 0; i < faceCount; i++ {
		var n uint8

		if err := binary.Read(reader, order, &n); err != nil {
			return Mesh{}, fmt.Errorf("invalid binary PLY face data")
		}

		if n < 3 {
			for j := 0; j < int(n); j++ {
				var dummy int32
				if err := binary.Read(reader, order, &dummy); err != nil {
					return Mesh{}, fmt.Errorf("invalid binary PLY face data")
				}
			}
			continue
		}

		indices := make([]int, int(n))

		for j := 0; j < int(n); j++ {
			var index int32

			if err := binary.Read(reader, order, &index); err != nil {
				return Mesh{}, fmt.Errorf("invalid binary PLY face data")
			}

			if index < 0 || int(index) >= len(vertices) {
				return Mesh{}, fmt.Errorf("binary PLY face index is invalid")
			}

			indices[j] = int(index)
		}

		for j := 1; j < len(indices)-1; j++ {
			builder.addTriangle(
				vertices[indices[0]],
				vertices[indices[j]],
				vertices[indices[j+1]],
			)
		}
	}

	return builder.mesh, nil
}

/* =========================================================
   3MF
   ========================================================= */

type threeMFModel struct {
	XMLName xml.Name `xml:"model"`
	Unit    string   `xml:"unit,attr"`

	Resources struct {
		Objects []struct {
			ID       int `xml:"id,attr"`
			MeshData struct {
				Vertices struct {
					Vertices []struct {
						X float64 `xml:"x,attr"`
						Y float64 `xml:"y,attr"`
						Z float64 `xml:"z,attr"`
					} `xml:"vertex"`
				} `xml:"vertices"`

				Triangles struct {
					Triangles []struct {
						V1 int `xml:"v1,attr"`
						V2 int `xml:"v2,attr"`
						V3 int `xml:"v3,attr"`
					} `xml:"triangle"`
				} `xml:"triangles"`
			} `xml:"mesh"`
		} `xml:"object"`
	} `xml:"resources"`
}

func parse3MF(data []byte) (Mesh, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Mesh{}, fmt.Errorf("invalid 3MF ZIP container: %w", err)
	}

	builder := newMeshBuilder("mm")

	foundModel := false

	for _, file := range reader.File {
		name := strings.ToLower(file.Name)

		if !strings.HasSuffix(name, ".model") {
			continue
		}

		rc, err := file.Open()
		if err != nil {
			continue
		}

		modelData, err := io.ReadAll(rc)
		_ = rc.Close()

		if err != nil {
			continue
		}

		var model threeMFModel

		if err := xml.Unmarshal(modelData, &model); err != nil {
			continue
		}

		unitMultiplier := threeMFUnitMultiplier(model.Unit)

		for _, object := range model.Resources.Objects {
			rawVertices := make([]Point, len(object.MeshData.Vertices.Vertices))

			for i, v := range object.MeshData.Vertices.Vertices {
				rawVertices[i] = Point{
					X: v.X * unitMultiplier,
					Y: v.Y * unitMultiplier,
					Z: v.Z * unitMultiplier,
				}
			}

			for _, t := range object.MeshData.Triangles.Triangles {
				if t.V1 < 0 ||
					t.V2 < 0 ||
					t.V3 < 0 ||
					t.V1 >= len(rawVertices) ||
					t.V2 >= len(rawVertices) ||
					t.V3 >= len(rawVertices) {
					continue
				}

				builder.addTriangle(
					rawVertices[t.V1],
					rawVertices[t.V2],
					rawVertices[t.V3],
				)
			}
		}

		foundModel = true
	}

	if !foundModel {
		return Mesh{}, fmt.Errorf("3MF model XML was not found")
	}

	if len(builder.mesh.Triangles) == 0 {
		return Mesh{}, fmt.Errorf("3MF contains no usable mesh")
	}

	return builder.mesh, nil
}

func threeMFUnitMultiplier(unit string) float64 {
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "", "millimeter", "millimetre", "mm":
		return 1

	case "micron", "micrometer", "micrometre":
		return 0.001

	case "centimeter", "centimetre", "cm":
		return 10

	case "meter", "metre", "m":
		return 1000

	case "inch", "in":
		return 25.4

	case "foot", "ft":
		return 304.8

	default:
		return 1
	}
}

/* =========================================================
   AMF
   ========================================================= */

type amfModel struct {
	XMLName xml.Name `xml:"amf"`
	Unit    string   `xml:"unit,attr"`

	Objects []struct {
		Mesh struct {
			Vertices struct {
				Vertices []struct {
					Coordinates struct {
						X float64 `xml:"x"`
						Y float64 `xml:"y"`
						Z float64 `xml:"z"`
					} `xml:"coordinates"`
				} `xml:"vertex"`
			} `xml:"vertices"`

			Volume []struct {
				Triangle []struct {
					V1 int `xml:"v1"`
					V2 int `xml:"v2"`
					V3 int `xml:"v3"`
				} `xml:"triangle"`
			} `xml:"volume"`
		} `xml:"mesh"`
	} `xml:"object"`
}

func parseAMF(data []byte) (Mesh, error) {
	var model amfModel

	if err := xml.Unmarshal(data, &model); err != nil {
		return Mesh{}, fmt.Errorf("invalid AMF XML: %w", err)
	}

	builder := newMeshBuilder("mm")

	multiplier := amfUnitMultiplier(model.Unit)

	for _, object := range model.Objects {
		rawVertices := make([]Point, len(object.Mesh.Vertices.Vertices))

		for i, v := range object.Mesh.Vertices.Vertices {
			rawVertices[i] = Point{
				X: v.Coordinates.X * multiplier,
				Y: v.Coordinates.Y * multiplier,
				Z: v.Coordinates.Z * multiplier,
			}
		}

		for _, volume := range object.Mesh.Volume {
			for _, t := range volume.Triangle {
				if t.V1 < 0 ||
					t.V2 < 0 ||
					t.V3 < 0 ||
					t.V1 >= len(rawVertices) ||
					t.V2 >= len(rawVertices) ||
					t.V3 >= len(rawVertices) {
					continue
				}

				builder.addTriangle(
					rawVertices[t.V1],
					rawVertices[t.V2],
					rawVertices[t.V3],
				)
			}
		}
	}

	if len(builder.mesh.Triangles) == 0 {
		return Mesh{}, fmt.Errorf("AMF contains no usable triangles")
	}

	return builder.mesh, nil
}

func amfUnitMultiplier(unit string) float64 {
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "", "millimeter", "millimetre", "mm":
		return 1

	case "centimeter", "centimetre", "cm":
		return 10

	case "meter", "metre", "m":
		return 1000

	case "inch", "in":
		return 25.4

	case "foot", "ft":
		return 304.8

	case "micron", "micrometer", "micrometre":
		return 0.001

	default:
		return 1
	}
}

/* =========================================================
   ROBUST MESH VOLUME
   ========================================================= */

type edgeKey struct {
	A int
	B int
}

type edgeUse struct {
	Triangles []int
	Dirs      []int
}

type componentInfo struct {
	Triangles []int
	Vertices  map[int]struct{}
	Volume    float64
	BBox      boundingBox
	Inside    Point
}

type boundingBox struct {
	Min   Point
	Max   Point
	Valid bool
}

func calculateRobustMeshVolume(mesh Mesh) (float64, int, bool, error) {
	if len(mesh.Vertices) == 0 {
		return 0, 0, false, fmt.Errorf("mesh has no vertices")
	}
	if len(mesh.Triangles) == 0 {
		return 0, 0, false, fmt.Errorf("mesh has no triangles")
	}

	// Use the mesh bounding-box center as the numerical origin.
	// This greatly reduces floating-point cancellation for models
	// whose coordinates are far from (0,0,0).
	var minX, minY, minZ float64
	var maxX, maxY, maxZ float64
	first := true

	for _, p := range mesh.Vertices {
		if !isFinitePoint(p) {
			return 0, 0, false, fmt.Errorf("mesh contains invalid vertex coordinates")
		}
		if first {
			minX, maxX = p.X, p.X
			minY, maxY = p.Y, p.Y
			minZ, maxZ = p.Z, p.Z
			first = false
			continue
		}
		if p.X < minX {
			minX = p.X
		}
		if p.X > maxX {
			maxX = p.X
		}
		if p.Y < minY {
			minY = p.Y
		}
		if p.Y > maxY {
			maxY = p.Y
		}
		if p.Z < minZ {
			minZ = p.Z
		}
		if p.Z > maxZ {
			maxZ = p.Z
		}
	}

	origin := Point{
		X: (minX + maxX) * 0.5,
		Y: (minY + maxY) * 0.5,
		Z: (minZ + maxZ) * 0.5,
	}

	var sum, compensation float64
	validTriangles := 0

	for i, tri := range mesh.Triangles {
		if tri.A < 0 || tri.A >= len(mesh.Vertices) ||
			tri.B < 0 || tri.B >= len(mesh.Vertices) ||
			tri.C < 0 || tri.C >= len(mesh.Vertices) {
			return 0, 0, false, fmt.Errorf("triangle %d contains invalid vertex index", i)
		}

		a := subtract(mesh.Vertices[tri.A], origin)
		b := subtract(mesh.Vertices[tri.B], origin)
		c := subtract(mesh.Vertices[tri.C], origin)

		// Ignore degenerate triangles.
		ab := subtract(b, a)
		ac := subtract(c, a)
		area2 := dot(cross(ab, ac), cross(ab, ac))
		if !isFiniteFloat(area2) {
			return 0, 0, false, fmt.Errorf("triangle %d contains invalid geometry", i)
		}
		if area2 <= epsArea {
			continue
		}

		value := dot(a, cross(b, c)) / 6.0
		if !isFiniteFloat(value) {
			return 0, 0, false, fmt.Errorf("triangle %d produced invalid volume", i)
		}

		// Neumaier-style compensated summation.
		y := value - compensation
		t := sum + y
		compensation = (t - sum) - y
		sum = t
		validTriangles++
	}

	if validTriangles == 0 {
		return 0, 0, false, fmt.Errorf("mesh contains no usable triangles")
	}

	volume := math.Abs(sum)
	if !isFiniteFloat(volume) || volume <= epsVolume {
		return 0, 0, false, fmt.Errorf("calculated model volume is zero or invalid")
	}

	// Deliberately avoid the old O(T) edge-map + adjacency + shell
	// containment pipeline. The volume path is now O(T) time and uses
	// only the existing mesh arrays. Geometry is never modified.
	return volume, 1, true, nil
}

func isFiniteFloat(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func isFinitePoint(p Point) bool {
	return isFiniteFloat(p.X) && isFiniteFloat(p.Y) && isFiniteFloat(p.Z)
}

/* =========================================================
   GEOMETRY HELPERS
   ========================================================= */

func makeEdgeKey(a, b int) edgeKey {
	if a < b {
		return edgeKey{
			A: a,
			B: b,
		}
	}

	return edgeKey{
		A: b,
		B: a,
	}
}

func signedTriangleVolumeRelative(a, b, c, origin Point) float64 {
	ra := subtract(a, origin)
	rb := subtract(b, origin)
	rc := subtract(c, origin)

	return dot(
		ra,
		cross(rb, rc),
	) / 6.0
}

func subtract(a, b Point) Point {
	return Point{
		X: a.X - b.X,
		Y: a.Y - b.Y,
		Z: a.Z - b.Z,
	}
}

func add(a, b Point) Point {
	return Point{
		X: a.X + b.X,
		Y: a.Y + b.Y,
		Z: a.Z + b.Z,
	}
}

func scale(a Point, s float64) Point {
	return Point{
		X: a.X * s,
		Y: a.Y * s,
		Z: a.Z * s,
	}
}

func cross(a, b Point) Point {
	return Point{
		X: a.Y*b.Z - a.Z*b.Y,
		Y: a.Z*b.X - a.X*b.Z,
		Z: a.X*b.Y - a.Y*b.X,
	}
}

func dot(a, b Point) float64 {
	return a.X*b.X + a.Y*b.Y + a.Z*b.Z
}

func length(a Point) float64 {
	return math.Sqrt(dot(a, a))
}

func normalize(a Point) Point {
	l := length(a)

	if l <= 1e-30 {
		return Point{}
	}

	return scale(a, 1/l)
}

func triangleAreaSquared(a, b, c Point) float64 {
	cr := cross(
		subtract(b, a),
		subtract(c, a),
	)

	return dot(cr, cr)
}

func (b *boundingBox) Add(p Point) {
	if !b.Valid {
		b.Min = p
		b.Max = p
		b.Valid = true
		return
	}

	b.Min.X = math.Min(b.Min.X, p.X)
	b.Min.Y = math.Min(b.Min.Y, p.Y)
	b.Min.Z = math.Min(b.Min.Z, p.Z)

	b.Max.X = math.Max(b.Max.X, p.X)
	b.Max.Y = math.Max(b.Max.Y, p.Y)
	b.Max.Z = math.Max(b.Max.Z, p.Z)
}

func (b boundingBox) Diagonal() float64 {
	if !b.Valid {
		return 0
	}

	return length(subtract(b.Max, b.Min))
}

func bboxContainsWithMargin(b boundingBox, p Point) bool {
	if !b.Valid {
		return false
	}

	margin := math.Max(b.Diagonal()*1e-9, 1e-9)

	return p.X >= b.Min.X-margin &&
		p.X <= b.Max.X+margin &&
		p.Y >= b.Min.Y-margin &&
		p.Y <= b.Max.Y+margin &&
		p.Z >= b.Min.Z-margin &&
		p.Z <= b.Max.Z+margin
}

/* =========================================================
   POINT INSIDE MESH
   ========================================================= */

func pointInsideComponent(
	p Point,
	mesh Mesh,
	component componentInfo,
) (bool, error) {
	/*
		Use a non-axis-aligned ray direction to reduce degeneracy
		with common CAD/STL faces.
	*/
	direction := normalize(Point{
		X: 1.0,
		Y: 0.3713906763541037,
		Z: 0.193847291,
	})

	hits := 0

	for _, triangleIndex := range component.Triangles {
		tri := mesh.Triangles[triangleIndex]

		a := mesh.Vertices[tri.A]
		b := mesh.Vertices[tri.B]
		c := mesh.Vertices[tri.C]

		if rayIntersectsTriangle(
			p,
			direction,
			a,
			b,
			c,
		) {
			hits++
		}
	}

	return hits%2 == 1, nil
}

func rayIntersectsTriangle(
	origin Point,
	direction Point,
	v0 Point,
	v1 Point,
	v2 Point,
) bool {
	const epsilon = 1e-12

	edge1 := subtract(v1, v0)
	edge2 := subtract(v2, v0)

	h := cross(direction, edge2)
	a := dot(edge1, h)

	if math.Abs(a) < epsilon {
		return false
	}

	f := 1.0 / a

	s := subtract(origin, v0)

	u := f * dot(s, h)

	if u < -epsilon || u > 1.0+epsilon {
		return false
	}

	q := cross(s, edge1)

	v := f * dot(direction, q)

	if v < -epsilon || u+v > 1.0+epsilon {
		return false
	}

	t := f * dot(edge2, q)

	return t > epsilon
}

/* =========================================================
   MATERIAL / WEIGHT
   ========================================================= */

func materialDensity(material string) float64 {
	switch strings.ToUpper(strings.TrimSpace(material)) {
	case "PLA":
		return 1.24

	case "PLA+":
		return 1.24

	case "PETG":
		return 1.27

	case "ABS":
		return 1.04

	case "ASA":
		return 1.07

	case "TPU":
		return 1.21

	case "NYLON":
		return 1.15

	default:
		return 1.24
	}
}

func estimatePrintWeight(
	solidWeight float64,
	infill float64,
) float64 {
	if infill < 0 {
		infill = 0
	}

	if infill > 100 {
		infill = 100
	}

	/*
		Rough print-material estimate.

		20% infill:
		0.15 + (0.85 * 0.20)
		= 0.32

		This is NOT a real slicer result.
	*/
	factor := 0.15 + (0.85 * infill / 100.0)

	return solidWeight * factor
}

/* =========================================================
   SHOPIFY
   ========================================================= */

func getShopifyAccessToken() (string, error) {
	shop := strings.TrimSpace(os.Getenv("SHOPIFY_SHOP"))
	clientID := strings.TrimSpace(os.Getenv("SHOPIFY_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("SHOPIFY_CLIENT_SECRET"))

	if shop == "" {
		return "", fmt.Errorf("SHOPIFY_SHOP environment variable is missing")
	}

	if clientID == "" {
		return "", fmt.Errorf("SHOPIFY_CLIENT_ID environment variable is missing")
	}

	if clientSecret == "" {
		return "", fmt.Errorf("SHOPIFY_CLIENT_SECRET environment variable is missing")
	}

	tokenURL := fmt.Sprintf(
		"https://%s.myshopify.com/admin/oauth/access_token",
		shop,
	)

	payload := map[string]string{
		"client_id":     clientID,
		"client_secret": clientSecret,
		"grant_type":    "client_credentials",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(
		http.MethodPost,
		tokenURL,
		bytes.NewReader(body),
	)
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf(
			"Shopify token request failed: HTTP %d: %s",
			resp.StatusCode,
			strings.TrimSpace(string(responseBody)),
		)
	}

	var tokenResponse struct {
		AccessToken string `json:"access_token"`
	}

	if err := json.Unmarshal(responseBody, &tokenResponse); err != nil {
		return "", err
	}

	if tokenResponse.AccessToken == "" {
		return "", fmt.Errorf("Shopify returned an empty access token")
	}

	return tokenResponse.AccessToken, nil
}

func createDraftOrder(
	request DraftOrderRequest,
) (string, error) {
	token, err := getShopifyAccessToken()
	if err != nil {
		return "", err
	}

	shop := strings.TrimSpace(os.Getenv("SHOPIFY_SHOP"))

	if shop == "" {
		return "", fmt.Errorf("SHOPIFY_SHOP is missing")
	}

	endpoint := fmt.Sprintf(
		"https://%s.myshopify.com/admin/api/%s/graphql.json",
		shop,
		shopifyAPI,
	)

	if request.Quantity < 1 {
		request.Quantity = 1
	}

	if request.Title == "" {
		request.Title = "Custom 3D Print"
	}

	lineItem := map[string]interface{}{
		"title":    request.Title,
		"quantity": request.Quantity,
		"originalUnitPriceWithCurrency": map[string]interface{}{
			"amount":       fmt.Sprintf("%.2f", request.Price),
			"currencyCode": "INR",
		},
	}

	metafields := []map[string]interface{}{
		{
			"namespace": "custom_print",
			"key":       "material",
			"value":     request.Material,
			"type":      "single_line_text_field",
		},
		{
			"namespace": "custom_print",
			"key":       "color",
			"value":     request.Color,
			"type":      "single_line_text_field",
		},
		{
			"namespace": "custom_print",
			"key":       "weight_g",
			"value":     fmt.Sprintf("%.4f", request.Weight),
			"type":      "number_decimal",
		},
		{
			"namespace": "custom_print",
			"key":       "volume_cm3",
			"value":     fmt.Sprintf("%.4f", request.Volume),
			"type":      "number_decimal",
		},
		{
			"namespace": "custom_print",
			"key":       "file_name",
			"value":     request.FileName,
			"type":      "single_line_text_field",
		},
	}

	if request.FileURL != "" {
		metafields = append(metafields, map[string]interface{}{
			"namespace": "custom_print",
			"key":       "file_url",
			"value":     request.FileURL,
			"type":      "single_line_text_field",
		})
	}

	if request.FileID != "" {
		metafields = append(metafields, map[string]interface{}{
			"namespace": "custom_print",
			"key":       "file_id",
			"value":     request.FileID,
			"type":      "single_line_text_field",
		})
	}

	variables := map[string]interface{}{
		"input": map[string]interface{}{
			"lineItems": []interface{}{
				lineItem,
			},
			"metafields": metafields,
		},
	}

	query := `
mutation DraftOrderCreate($input: DraftOrderInput!) {
  draftOrderCreate(input: $input) {
    draftOrder {
      id
      invoiceUrl
      status
    }
    userErrors {
      field
      message
    }
  }
}
`

	payload := map[string]interface{}{
		"query":     query,
		"variables": variables,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(
		http.MethodPost,
		endpoint,
		bytes.NewReader(body),
	)
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Shopify-Access-Token", token)

	client := &http.Client{
		Timeout: 45 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf(
			"Shopify Draft Order API returned HTTP %d: %s",
			resp.StatusCode,
			strings.TrimSpace(string(responseBody)),
		)
	}

	var graphQL GraphQLResponse

	if err := json.Unmarshal(responseBody, &graphQL); err != nil {
		return "", fmt.Errorf(
			"invalid Shopify GraphQL response: %w",
			err,
		)
	}

	if len(graphQL.Errors) > 0 {
		return "", fmt.Errorf(
			"Shopify GraphQL error: %s",
			graphQL.Errors[0].Message,
		)
	}

	var result struct {
		DraftOrderCreate struct {
			DraftOrder struct {
				ID         string `json:"id"`
				InvoiceURL string `json:"invoiceUrl"`
				Status     string `json:"status"`
			} `json:"draftOrder"`

			UserErrors []struct {
				Field   []string `json:"field"`
				Message string   `json:"message"`
			} `json:"userErrors"`
		} `json:"draftOrderCreate"`
	}

	if err := json.Unmarshal(graphQL.Data, &result); err != nil {
		return "", fmt.Errorf(
			"invalid Shopify draft order data: %w",
			err,
		)
	}

	if len(result.DraftOrderCreate.UserErrors) > 0 {
		return "", fmt.Errorf(
			"Shopify draft order error: %s",
			result.DraftOrderCreate.UserErrors[0].Message,
		)
	}

	invoiceURL := result.DraftOrderCreate.DraftOrder.InvoiceURL

	if invoiceURL == "" {
		return "", fmt.Errorf(
			"Shopify created the draft order but did not return a checkout URL",
		)
	}

	return invoiceURL, nil
}

func createDraftOrderHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"success": false,
			"error":   "POST method required",
		})
		return
	}

	var request DraftOrderRequest

	decoder := json.NewDecoder(io.LimitReader(
		r.Body,
		2<<20,
	))

	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Invalid JSON: " + err.Error(),
		})
		return
	}

	if request.Price <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Price must be greater than zero",
		})
		return
	}

	if request.Quantity < 1 {
		request.Quantity = 1
	}

	checkoutURL, err := createDraftOrder(request)

	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":      true,
		"checkout_url": checkoutURL,
	})
}

/* =========================================================
   HELPERS
   ========================================================= */

func minInt(a, b int) int {
	if a < b {
		return a
	}

	return b
}

/*
	Keep these references used by some future integrations.
*/

var _ = add
var _ = url.QueryEscape

/*
Prevent an unused sync import if future upload locking is enabled.
*/
var _ sync.Mutex
