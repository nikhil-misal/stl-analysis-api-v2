package stlengine

import "math"

// signedTetraVolume6 returns 6x the signed volume of the tetrahedron formed
// by the reference origin o and triangle (a,b,c):
//
//	6V = (a-o) . ((b-o) x (c-o))
//
// Summing this over every triangle of a closed, consistently-oriented mesh
// and dividing by 6 gives the exact enclosed volume, independent of the
// choice of o (any two choices of o differ by a constant that cancels out
// over a closed surface). We exploit that: instead of using the world
// origin (which can be millions of units away from the actual model,
// destroying precision because a-o, b-o, c-o become huge numbers whose
// small pairwise differences are what actually matters), we use the very
// first vertex encountered in the stream as o. That point is guaranteed to
// lie ON the model, so every subsequent (a-o) etc. is already a small,
// well-scaled number. This gives the numerical benefit of a "local origin"
// strategy with zero extra passes and O(1) extra memory.
func signedTetraVolume6(o, a, b, c Vec3) float64 {
	ax, ay, az := a.X-o.X, a.Y-o.Y, a.Z-o.Z
	bx, by, bz := b.X-o.X, b.Y-o.Y, b.Z-o.Z
	cx, cy, cz := c.X-o.X, c.Y-o.Y, c.Z-o.Z

	crossX := by*cz - bz*cy
	crossY := bz*cx - bx*cz
	crossZ := bx*cy - by*cx

	return ax*crossX + ay*crossY + az*crossZ
}

// triangleAreaSquaredX4 returns 4x the squared area of triangle (a,b,c),
// i.e. |cross(b-a, c-a)|^2. Used only for the cheap zero-area / degenerate
// check; comparing squared magnitudes avoids a sqrt in the hot loop.
func triangleAreaSquaredX4(a, b, c Vec3) float64 {
	ux, uy, uz := b.X-a.X, b.Y-a.Y, b.Z-a.Z
	vx, vy, vz := c.X-a.X, c.Y-a.Y, c.Z-a.Z
	cx := uy*vz - uz*vy
	cy := uz*vx - ux*vz
	cz := ux*vy - uy*vx
	return cx*cx + cy*cy + cz*cz
}

const zeroAreaThresholdX4 = 1e-20

// vKey is a quantized vertex identity used ONLY for topology (manifold /
// component / cavity) analysis. It is never used for the volume
// calculation, which always uses exact input coordinates. Coordinates are
// quantized relative to the same local origin used for the volume sum, so
// models translated far from the world origin quantize just as reliably as
// models sitting at (0,0,0). See Options.VertexWeldEpsilon.
type vKey struct {
	X, Y, Z int64
}

func quantize(p, origin Vec3, eps float64) vKey {
	inv := 1.0 / eps
	return vKey{
		X: int64(math.Round((p.X - origin.X) * inv)),
		Y: int64(math.Round((p.Y - origin.Y) * inv)),
		Z: int64(math.Round((p.Z - origin.Z) * inv)),
	}
}
