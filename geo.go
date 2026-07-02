// Geo-spatial index over (latitude, longitude) node properties. Points
// embed on the unit sphere (lat/lon -> 3-D Cartesian), so Euclidean chord
// distance is monotonic with great-circle distance and a 3-D k-d tree
// answers radius and k-NN queries exactly -- correct at the poles and
// across the antimeridian. Distances report in kilometres.

package chickpeas

import (
	"container/heap"
	"math"
	"sort"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/freeeve/gochickpeas/nodeset"
)

// earthRadiusKM is the mean Earth radius (IUGG).
const earthRadiusKM = 6371.0088

type kdNode struct {
	point       int
	left, right *kdNode
}

// GeoIndex is the immutable index for one (label, latKey, lonKey) field.
type GeoIndex struct {
	nodes []uint32
	lats  []float64
	lons  []float64
	xyz   [][3]float64
	root  *kdNode
}

// BuildGeoIndex builds from (node, latDeg, lonDeg) points; non-finite or
// out-of-range coordinates are skipped.
func BuildGeoIndex(points func(yield func(node uint32, lat, lon float64) bool)) *GeoIndex {
	idx := &GeoIndex{}
	points(func(node uint32, lat, lon float64) bool {
		if math.IsNaN(lat) || math.IsInf(lat, 0) || math.IsNaN(lon) || math.IsInf(lon, 0) ||
			lat < -90 || lat > 90 || lon < -180 || lon > 180 {
			return true
		}
		idx.nodes = append(idx.nodes, node)
		idx.lats = append(idx.lats, lat)
		idx.lons = append(idx.lons, lon)
		idx.xyz = append(idx.xyz, unitVec(lat, lon))
		return true
	})
	indices := make([]int, len(idx.nodes))
	for i := range indices {
		indices[i] = i
	}
	idx.root = buildKD(idx.xyz, indices, 0)
	return idx
}

// Len is the number of indexed points.
func (g *GeoIndex) Len() int { return len(g.nodes) }

// WithinRadius returns the nodes within km great-circle distance of
// (lat, lon).
func (g *GeoIndex) WithinRadius(lat, lon, km float64) *nodeset.Set {
	if km < 0 || g.root == nil {
		return nodeset.New()
	}
	q := unitVec(lat, lon)
	chord := chordThreshold(km)
	out := roaring.New()
	g.rangeSearch(g.root, &q, chord*chord, 0, out)
	return nodeset.FromBitmap(out)
}

// GeoHit is one KNN result.
type GeoHit struct {
	Node uint32
	KM   float64
}

// KNN returns up to k nearest nodes to (lat, lon), sorted by increasing
// distance, ties by ascending node id.
func (g *GeoIndex) KNN(lat, lon float64, k int) []GeoHit {
	if k <= 0 || g.root == nil {
		return nil
	}
	q := unitVec(lat, lon)
	h := &neighborHeap{}
	g.nn(g.root, &q, k, 0, h)
	out := make([]GeoHit, 0, h.Len())
	for _, n := range *h {
		out = append(out, GeoHit{Node: n.node, KM: chordToKM(math.Sqrt(n.distSq))})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].KM != out[j].KM {
			return out[i].KM < out[j].KM
		}
		return out[i].Node < out[j].Node
	})
	return out
}

// WithinBBox returns the nodes inside the lat/lon rectangle; minLon >
// maxLon treats the box as crossing the antimeridian.
func (g *GeoIndex) WithinBBox(minLat, minLon, maxLat, maxLon float64) *nodeset.Set {
	lonIn := func(lon float64) bool {
		if minLon <= maxLon {
			return lon >= minLon && lon <= maxLon
		}
		return lon >= minLon || lon <= maxLon
	}
	out := roaring.New()
	for i, node := range g.nodes {
		if g.lats[i] >= minLat && g.lats[i] <= maxLat && lonIn(g.lons[i]) {
			out.Add(node)
		}
	}
	return nodeset.FromBitmap(out)
}

func (g *GeoIndex) rangeSearch(n *kdNode, q *[3]float64, rSq float64, axis int, out *roaring.Bitmap) {
	if n == nil {
		return
	}
	p := n.point
	if distSq(&g.xyz[p], q) <= rSq {
		out.Add(g.nodes[p])
	}
	diff := q[axis] - g.xyz[p][axis]
	next := (axis + 1) % 3
	near, far := n.left, n.right
	if diff > 0 {
		near, far = n.right, n.left
	}
	g.rangeSearch(near, q, rSq, next, out)
	if diff*diff <= rSq {
		g.rangeSearch(far, q, rSq, next, out)
	}
}

func (g *GeoIndex) nn(n *kdNode, q *[3]float64, k, axis int, h *neighborHeap) {
	if n == nil {
		return
	}
	p := n.point
	pushBounded(h, k, neighbor{distSq: distSq(&g.xyz[p], q), node: g.nodes[p]})
	diff := q[axis] - g.xyz[p][axis]
	next := (axis + 1) % 3
	near, far := n.left, n.right
	if diff > 0 {
		near, far = n.right, n.left
	}
	g.nn(near, q, k, next, h)
	worst := math.Inf(1)
	if h.Len() >= k {
		worst = (*h)[0].distSq
	}
	if diff*diff <= worst {
		g.nn(far, q, k, next, h)
	}
}

// neighborHeap is a max-heap by (distance, node id) so its root is the
// current worst of the k best.
type neighbor struct {
	distSq float64
	node   uint32
}

type neighborHeap []neighbor

func (h neighborHeap) Len() int { return len(h) }
func (h neighborHeap) Less(i, j int) bool {
	if h[i].distSq != h[j].distSq {
		return h[i].distSq > h[j].distSq
	}
	return h[i].node > h[j].node
}
func (h neighborHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *neighborHeap) Push(x any)   { *h = append(*h, x.(neighbor)) }
func (h *neighborHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

func pushBounded(h *neighborHeap, k int, n neighbor) {
	heap.Push(h, n)
	if h.Len() > k {
		heap.Pop(h)
	}
}

// buildKD builds a balanced tree by median split (axis = depth % 3),
// partitioning around the median in O(n) per level.
func buildKD(xyz [][3]float64, indices []int, depth int) *kdNode {
	if len(indices) == 0 {
		return nil
	}
	axis := depth % 3
	mid := len(indices) / 2
	selectNth(xyz, indices, mid, axis)
	return &kdNode{
		point: indices[mid],
		left:  buildKD(xyz, indices[:mid], depth+1),
		right: buildKD(xyz, indices[mid+1:], depth+1),
	}
}

// selectNth partitions indices so indices[nth] holds the nth element by
// the axis coordinate (quickselect).
func selectNth(xyz [][3]float64, indices []int, nth, axis int) {
	lo, hi := 0, len(indices)-1
	for lo < hi {
		pivot := xyz[indices[(lo+hi)/2]][axis]
		i, j := lo, hi
		for i <= j {
			for xyz[indices[i]][axis] < pivot {
				i++
			}
			for xyz[indices[j]][axis] > pivot {
				j--
			}
			if i <= j {
				indices[i], indices[j] = indices[j], indices[i]
				i++
				j--
			}
		}
		if nth <= j {
			hi = j
		} else if nth >= i {
			lo = i
		} else {
			return
		}
	}
}

func unitVec(lat, lon float64) [3]float64 {
	la, lo := lat*math.Pi/180, lon*math.Pi/180
	cosLat := math.Cos(la)
	return [3]float64{cosLat * math.Cos(lo), cosLat * math.Sin(lo), math.Sin(la)}
}

func distSq(a, b *[3]float64) float64 {
	dx, dy, dz := a[0]-b[0], a[1]-b[1], a[2]-b[2]
	return dx*dx + dy*dy + dz*dz
}

// chordThreshold is the unit-sphere chord length for a great-circle
// distance of km.
func chordThreshold(km float64) float64 {
	theta := math.Min(km/earthRadiusKM, math.Pi)
	return 2 * math.Sin(theta/2)
}

// chordToKM inverts chordThreshold.
func chordToKM(chord float64) float64 {
	return 2 * math.Asin(math.Max(-1, math.Min(1, chord/2))) * earthRadiusKM
}

// HaversineKM is the great-circle distance in kilometres between two
// lat/lon points -- a public utility independent of the index.
func HaversineKM(lat1, lon1, lat2, lon2 float64) float64 {
	p1, p2 := lat1*math.Pi/180, lat2*math.Pi/180
	dlat := (lat2 - lat1) * math.Pi / 180
	dlon := (lon2 - lon1) * math.Pi / 180
	sinLat := math.Sin(dlat / 2)
	sinLon := math.Sin(dlon / 2)
	a := sinLat*sinLat + math.Cos(p1)*math.Cos(p2)*sinLon*sinLon
	return 2 * math.Asin(math.Max(-1, math.Min(1, math.Sqrt(a)))) * earthRadiusKM
}

// ln32 is float32 natural log via float64.
func ln32(x float32) float32 {
	return float32(math.Log(float64(x)))
}
