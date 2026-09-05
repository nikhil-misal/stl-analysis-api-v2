package stlengine

import (
	"bufio"
	"context"
	"fmt"
	"io"
)

const peekWindow = 8192

// Analyze streams an STL file from rs (a seekable reader over `size` bytes
// — in practice the *os.File backing the request's temp upload) and
// produces a Result without ever loading the file into memory or building
// a full in-memory mesh.
//
// Only one pass is made over the triangle data. Volume, bounding box, cheap
// validation, and (when the file is within Options.TopologyBudget) manifold
// and component analysis are all accumulated together as triangles stream
// past.
//
// Analyze returns a non-nil error only when NO result could be produced at
// all (unrecognized/corrupt/truncated/empty file, IO error, context
// cancellation, or the file exceeds Options.MaxTriangles). A geometrically
// valid-but-unreliable mesh (open, non-manifold, ambiguous nesting) is NOT
// an error: it is returned as a Result with VolumeReliable=false and a
// MeshStatus/ErrorCode explaining why, per the "never fake a result, but
// don't reject on complexity alone" requirement.
func Analyze(ctx context.Context, rs io.ReadSeeker, size int64, opts Options) (Result, error) {
	opts = opts.withDefaults()

	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return Result{}, fmt.Errorf("%w: %v", errInvalid(ErrInternal), err)
	}

	peekLen := int64(peekWindow)
	if size < peekLen {
		peekLen = size
	}
	peek := make([]byte, peekLen)
	if peekLen > 0 {
		if _, err := io.ReadFull(rs, peek); err != nil && err != io.ErrUnexpectedEOF {
			return Result{}, fmt.Errorf("%w: %v", errInvalid(ErrInternal), err)
		}
	}

	format, triCountHint, err := detectFormat(peek, size)
	if err != nil {
		return Result{MeshStatus: StatusUnsupported, ErrorCode: CodeOf(err), ErrorMessage: err.Error()}, err
	}

	if format == "binary" && uint64(triCountHint) > opts.MaxTriangles {
		err := fmt.Errorf("%w: file declares %d triangles, above the configured limit of %d",
			errInvalid(ErrFileTooLarge), triCountHint, opts.MaxTriangles)
		return Result{Format: format, MeshStatus: StatusUnsupported, ErrorCode: ErrFileTooLarge, ErrorMessage: err.Error()}, err
	}

	var startOffset int64
	if format == "binary" {
		startOffset = binaryHeaderSize
	}
	if _, err := rs.Seek(startOffset, io.SeekStart); err != nil {
		return Result{}, fmt.Errorf("%w: %v", errInvalid(ErrInternal), err)
	}
	br := bufio.NewReaderSize(rs, 1<<20)

	var (
		bbox          BoundingBox
		volumeSum     neumaierSum
		triangleCount uint64
		nonFinite     uint64
		zeroArea      uint64
		degenerate    uint64

		origin      Vec3
		originSet   bool
		tracker     *topologyTracker
		trackerLive = true
	)

	if format == "binary" && uint64(triCountHint) > opts.TopologyBudget {
		// Known ahead of time: skip building the topology map entirely
		// rather than building it partway and throwing it away.
		trackerLive = false
	}

	visit := func(a, b, c Vec3) error {
		triangleCount++

		if opts.MaxTriangles > 0 && triangleCount > opts.MaxTriangles {
			return fmt.Errorf("%w: exceeds configured triangle limit (%d)",
				errInvalid(ErrFileTooLarge), opts.MaxTriangles)
		}

		if triangleCount%50000 == 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("%w: %v", errInvalid(ErrProcessingTimeout), ctx.Err())
			default:
			}
		}

		if !pointFinite(a) || !pointFinite(b) || !pointFinite(c) {
			nonFinite++
			return nil // excluded from volume/bbox/topology; sums must never see NaN/Inf
		}

		if !originSet {
			origin, originSet = a, true
		}

		bbox.Add(a)
		bbox.Add(b)
		bbox.Add(c)

		if a == b || b == c || c == a {
			degenerate++
			return nil
		}
		if triangleAreaSquaredX4(a, b, c) <= zeroAreaThresholdX4 {
			zeroArea++
			return nil
		}

		v6 := signedTetraVolume6(origin, a, b, c)
		volumeSum.Add(v6 / 6.0)

		if trackerLive {
			if tracker == nil {
				tracker = newTopologyTracker(origin, opts.VertexWeldEpsilon)
			}
			var triBox BoundingBox
			triBox.Add(a)
			triBox.Add(b)
			triBox.Add(c)
			tracker.addTriangle(a, b, c, v6, triBox)

			if triangleCount > opts.TopologyBudget {
				// Crossed the budget mid-stream (only possible for ASCII,
				// where we don't have a triangle-count hint up front).
				// Partial topology can't answer "is this closed", so drop
				// it rather than keep paying for it.
				trackerLive = false
				tracker = nil
			}
		}
		return nil
	}

	var streamErr error
	switch format {
	case "binary":
		streamErr = streamBinarySTL(br, triCountHint, visit)
	case "ascii":
		_, streamErr = streamASCIISTL(br, visit)
	default:
		streamErr = fmt.Errorf("%w: unrecognized format %q", errInvalid(ErrInvalidFile), format)
	}

	if streamErr != nil {
		return Result{
			Format:        format,
			TriangleCount: triangleCount,
			MeshStatus:    StatusUnsupported,
			ErrorCode:     CodeOf(streamErr),
			ErrorMessage:  streamErr.Error(),
		}, streamErr
	}

	validTriangles := triangleCount - nonFinite - degenerate - zeroArea
	if triangleCount == 0 || validTriangles == 0 {
		err := fmt.Errorf("%w: no usable geometry found (%d triangles read, %d usable)",
			errInvalid(ErrEmptyModel), triangleCount, validTriangles)
		return Result{
			Format:              format,
			TriangleCount:       triangleCount,
			NonFiniteTriangles:  nonFinite,
			ZeroAreaTriangles:   zeroArea,
			DegenerateTriangles: degenerate,
			MeshStatus:          StatusEmpty,
			ErrorCode:           ErrEmptyModel,
			ErrorMessage:        err.Error(),
		}, err
	}

	res := Result{
		Format:              format,
		TriangleCount:       triangleCount,
		Dimensions:          bbox,
		NonFiniteTriangles:  nonFinite,
		ZeroAreaTriangles:   zeroArea,
		DegenerateTriangles: degenerate,
	}
	rawVolume := absFloat(volumeSum.Value())

	if nonFinite > 0 {
		res.MeshStatus = StatusSuspicious
		res.VolumeReliable = false
		res.ErrorCode = ErrInvalidCoordinates
		res.ErrorMessage = fmt.Sprintf(
			"%d of %d triangles contained non-finite (NaN/Inf) coordinates and were excluded; the calculated volume cannot be trusted",
			nonFinite, triangleCount)
		return res, nil
	}

	if tracker != nil {
		res.TopologyEvaluated = true
		manifold := tracker.summarizeManifold()
		components := tracker.components()

		switch {
		case manifold.NonManifoldEdges > 0 || manifold.FlippedEdges > 0:
			res.Components = components
			res.VolumeMM3 = rawVolume
			res.MeshStatus = StatusNonManifold
			res.VolumeReliable = false
			res.ErrorCode = ErrNonManifold
			res.ErrorMessage = "the mesh has an edge shared by more than two triangles, or two triangles meeting an edge with inconsistent winding; this is not a simple manifold surface and the volume is not reliable"
		case manifold.OpenEdges > 0:
			res.Components = components
			res.VolumeMM3 = rawVolume
			res.MeshStatus = StatusOpen
			res.VolumeReliable = false
			res.ErrorCode = ErrOpenMesh
			res.ErrorMessage = "the mesh has open (boundary) edges; it is not watertight, so the calculated volume is not a reliable physical volume"
		default:
			netVolume, ambiguous := classifyShells(components)
			res.Components = components
			res.VolumeMM3 = netVolume
			if len(components) > 1 {
				res.MeshStatus = StatusMultiShell
			} else {
				res.MeshStatus = StatusClosed
			}
			if ambiguous {
				res.MeshStatus = StatusSuspicious
				res.VolumeReliable = false
				res.ErrorCode = ErrVolumeUnreliable
				res.ErrorMessage = "multiple shells overlap without one cleanly containing the other; automatic cavity detection could not confirm solid material vs. void for all shells"
			} else {
				res.VolumeReliable = true
			}
		}
	} else {
		res.TopologyEvaluated = false
		res.MeshStatus = StatusSuspicious
		res.VolumeMM3 = rawVolume
		res.VolumeReliable = false
		res.ErrorCode = ErrVolumeUnreliable
		res.ErrorMessage = fmt.Sprintf(
			"model has %d triangles, above the topology verification budget (%d); volume is reported but watertightness could not be verified within the configured memory budget",
			triangleCount, opts.TopologyBudget)
	}

	return res, nil
}
