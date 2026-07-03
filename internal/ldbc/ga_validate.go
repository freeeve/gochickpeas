// Reference-output validation for the six Graphalytics algorithms --
// port of rustychickpeas-ldbc src/graphalytics/validate.rs. Outputs are
// node-indexed; reference files are `<vertex-id> <value>` per line, so
// each check maps node -> vertex via the dataset before comparing.
// Modes follow the spec: exact (BFS/CDLP), relabel-invariant (WCC),
// tolerance (PageRank/LCC/SSSP).

package ldbc

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParseGAReference parses a `<vertex-id> <value>` reference file into a
// vertex -> raw-value map.
func ParseGAReference(text string) map[uint32]string {
	m := map[uint32]string{}
	for line := range strings.Lines(text) {
		v, val, _ := threeFields(line)
		if v == "" || val == "" {
			continue
		}
		if vid, err := strconv.ParseUint(v, 10, 32); err == nil {
			m[uint32(vid)] = val
		}
	}
	return m
}

// gaRefValue looks a vertex up in the reference with descriptive errors.
func gaRefValue(reference map[uint32]string, vertex uint32) (string, error) {
	raw, ok := reference[vertex]
	if !ok {
		return "", fmt.Errorf("vertex %d missing from reference", vertex)
	}
	return raw, nil
}

// GACheckExactI64 requires exact integer agreement (BFS depths, CDLP
// labels), reporting the first disagreeing vertex.
func GACheckExactI64(ds *GADataset, out []int64, reference map[uint32]string) error {
	for node, mine := range out {
		vertex := ds.VertexOfNode[node]
		raw, err := gaRefValue(reference, vertex)
		if err != nil {
			return err
		}
		want, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("vertex %d: unparseable reference value %q", vertex, raw)
		}
		if mine != want {
			return fmt.Errorf("vertex %d: got %d, want %d", vertex, mine, want)
		}
	}
	return nil
}

// GACheckEpsilon requires float agreement within eps (PageRank, LCC,
// SSSP); both-infinite (same sign) passes, mixed finite/infinite fails.
func GACheckEpsilon(ds *GADataset, out []float64, reference map[uint32]string, eps float64) error {
	for node, mine := range out {
		vertex := ds.VertexOfNode[node]
		raw, err := gaRefValue(reference, vertex)
		if err != nil {
			return err
		}
		want, err := strconv.ParseFloat(strings.ToLower(raw), 64)
		if err != nil {
			return fmt.Errorf("vertex %d: unparseable reference value %q", vertex, raw)
		}
		ok := false
		if math.IsInf(mine, 0) || math.IsInf(want, 0) {
			ok = math.IsInf(mine, 0) && math.IsInf(want, 0) && (mine > 0) == (want > 0)
		} else {
			ok = math.Abs(mine-want) <= eps
		}
		if !ok {
			return fmt.Errorf("vertex %d: got %v, want %v (eps %v)", vertex, mine, want, eps)
		}
	}
	return nil
}

// GACheckRelabel requires the partition out induces over vertices to
// equal the reference's regardless of label values (WCC), enforcing a
// consistent bijection between label spaces.
func GACheckRelabel(ds *GADataset, out []uint32, reference map[uint32]string) error {
	oursToRef := map[uint32]string{}
	refToOurs := map[string]uint32{}
	for node, mine := range out {
		vertex := ds.VertexOfNode[node]
		want, err := gaRefValue(reference, vertex)
		if err != nil {
			return err
		}
		if prev, seen := oursToRef[mine]; seen {
			if prev != want {
				return fmt.Errorf("vertex %d: our label %d maps to both %s and %s", vertex, mine, prev, want)
			}
			continue
		}
		if other, seen := refToOurs[want]; seen && other != mine {
			return fmt.Errorf("vertex %d: ref label %s maps to both %d and %d", vertex, want, other, mine)
		}
		oursToRef[mine] = want
		refToOurs[want] = mine
	}
	return nil
}
