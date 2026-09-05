package stlengine

import "sort"

// classifyShells decides, for a small number of connected components C
// (typically single digits, occasionally up to a few hundred for things
// like a tray of miniatures), which shells represent solid material and
// which represent cavities nested inside another shell.
//
// This is deliberately O(C^2) bounding-box containment checks, NOT O(N^2)
// over triangles — C is the shell count, N is the triangle count, and for
// ordinary uploads C is 1 (skipped entirely below). Even a customer file
// with a few hundred disconnected shells costs at most tens of thousands of
// bbox comparisons, which is negligible next to the O(N) triangle pass that
// already happened.
//
// Nesting parity rule: a shell nested inside an odd number of larger shells
// is a cavity (its volume is subtracted); nested inside an even number
// (including zero) it is solid material (its volume is added). This
// correctly handles double-nested cases (an island of material sitting
// inside a hollow inside a solid), not just the common single-cavity case.
//
// This is a bounding-box heuristic, not exact point-in-mesh containment.
// It is correct for the overwhelmingly common real-world case (a hollow
// shape's cavity is a smaller, non-overlapping shell sitting inside the
// outer one). For the rare case of two shells whose bounding boxes overlap
// without one cleanly containing the other (e.g. two interlocking,
// non-intersecting parts), the engine reports ambiguous=true and the
// caller marks the result as suspicious/unreliable rather than guessing.
// A true point-in-mesh test (ray casting against a spatial grid of the
// candidate outer shell's triangles) is a well-defined next step if this
// case turns out to matter for real customer files — see
// ARCHITECTURE.md "Known simplifications" — but it requires re-reading the
// specific shells from the temp file and was left out of this pass so the
// shipped code could be kept small enough to review and test carefully.
func classifyShells(components []ComponentInfo) (netVolumeMM3 float64, ambiguous bool) {
	if len(components) == 0 {
		return 0, false
	}
	if len(components) == 1 {
		return components[0].VolumeMM3, false
	}

	order := make([]int, len(components))
	for i := range order {
		order[i] = i
	}
	// Sort by bbox volume descending so containment checks read naturally,
	// though the algorithm below doesn't depend on this order.
	sort.Slice(order, func(i, j int) bool {
		return components[order[i]].Bounds.Volume() > components[order[j]].Bounds.Volume()
	})

	depth := make([]int, len(components))
	for _, i := range order {
		for _, j := range order {
			if i == j {
				continue
			}
			bi, bj := components[i].Bounds, components[j].Bounds
			if bj.Volume() < bi.Volume() {
				continue // only larger-or-equal shells are candidates to "contain" i
			}
			if bj.Volume() == bi.Volume() && j < i {
				continue // stable tie-break
			}
			if bj.Contains(bi) {
				depth[i]++
			} else if bj.Overlaps(bi) {
				// Overlapping without containment: our bbox heuristic can't
				// classify this shell with confidence.
				ambiguous = true
			}
		}
	}

	var sum neumaierSum
	for i, c := range components {
		v := c.VolumeMM3
		if depth[i]%2 == 1 {
			v = -v
			components[i].IsCavity = true
		}
		sum.Add(v)
	}
	return absFloat(sum.Value()), ambiguous
}
