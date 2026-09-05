package stlengine

import (
	"bytes"
	"context"
	"testing"
)

// n chosen so that 12*n^2 lands close to the target triangle count.
var benchScales = []struct {
	name string
	n    int
}{
	{"1k", 9},     // 972 triangles
	{"10k", 29},   // 10,092 triangles
	{"100k", 91},  // 99,372 triangles
	{"500k", 204}, // 499,392 triangles — the scale of the known problem file
	{"1M", 289},   // 1,002,252 triangles
}

func BenchmarkAnalyzeBinary(b *testing.B) {
	for _, scale := range benchScales {
		scale := scale
		b.Run(scale.name, func(b *testing.B) {
			tris := tessellateCube(100, Vec3{}, scale.n)
			data := buildBinarySTL(tris)
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r := bytes.NewReader(data)
				if _, err := Analyze(context.Background(), r, int64(len(data)), DefaultOptions()); err != nil {
					b.Fatalf("analyze failed: %v", err)
				}
			}
		})
	}
}

// BenchmarkAnalyzeBinaryNoTopology isolates the cost of the fast path alone
// (volume + bbox + cheap checks) by setting TopologyBudget to 0 triangles,
// forcing every scale to skip manifold/component analysis. Compare against
// BenchmarkAnalyzeBinary to see the topology pass's marginal cost.
func BenchmarkAnalyzeBinaryNoTopology(b *testing.B) {
	opts := DefaultOptions()
	opts.TopologyBudget = 1 // effectively disables it for all but trivial files
	for _, scale := range benchScales {
		scale := scale
		b.Run(scale.name, func(b *testing.B) {
			tris := tessellateCube(100, Vec3{}, scale.n)
			data := buildBinarySTL(tris)
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r := bytes.NewReader(data)
				if _, err := Analyze(context.Background(), r, int64(len(data)), opts); err != nil {
					b.Fatalf("analyze failed: %v", err)
				}
			}
		})
	}
}

func BenchmarkAnalyzeASCII(b *testing.B) {
	// ASCII files are much larger per-triangle; only go up to 100k here so
	// `go test -bench` doesn't take minutes by default. Use -run=NONE
	// -bench=ASCII/1M -benchtime=1x manually for the bigger scales.
	scales := benchScales[:3]
	for _, scale := range scales {
		scale := scale
		b.Run(scale.name, func(b *testing.B) {
			tris := tessellateCube(100, Vec3{}, scale.n)
			data := buildASCIISTL(tris)
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r := bytes.NewReader(data)
				if _, err := Analyze(context.Background(), r, int64(len(data)), DefaultOptions()); err != nil {
					b.Fatalf("analyze failed: %v", err)
				}
			}
		})
	}
}
