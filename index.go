// Lazily built inverted search indexes on the Snapshot: full-text (BM25)
// and geo-spatial (k-d tree). Each (label, field) index builds on first
// query and caches; the shared index is cloned out under a brief lock so
// the query itself runs off the mutex, and a racing build is discarded.

package chickpeas

import "github.com/freeeve/gochickpeas/nodeset"

type geoKey struct {
	label  Label
	latKey PropertyKey
	lonKey PropertyKey
}

// FullTextSearch returns the nodes of label whose key string property
// contains every token in query (boolean AND); empty for an unknown
// label/key, an empty query, or a token no document contains. The result
// composes with NodesWithLabel and other sets via And/Or/AndNot.
func (g *Snapshot) FullTextSearch(label, key, query string) *nodeset.Set {
	field, ok := g.fulltextField(label, key)
	if !ok {
		return nodeset.New()
	}
	return field.Query(query)
}

// FullTextSearchRanked returns the top k nodes of label by BM25 relevance
// of their key property to query (disjunctive), sorted by score
// descending, ties by ascending node id.
func (g *Snapshot) FullTextSearchRanked(label, key, query string, k int) []RankedHit {
	field, ok := g.fulltextField(label, key)
	if !ok {
		return nil
	}
	return field.QueryRanked(query, k)
}

func (g *Snapshot) fulltextField(label, key string) (*FullTextField, bool) {
	labelID, ok := g.Label(label)
	if !ok {
		return nil, false
	}
	keyID, ok := g.PropertyKey(key)
	if !ok {
		return nil, false
	}
	cacheKey := propIndexKey{label: labelID, key: keyID}
	g.fulltextMu.Lock()
	cached, ok := g.fulltextIndex[cacheKey]
	g.fulltextMu.Unlock()
	if ok {
		return cached, true
	}
	built := g.buildFullTextField(labelID, keyID)
	g.fulltextMu.Lock()
	defer g.fulltextMu.Unlock()
	if existing, ok := g.fulltextIndex[cacheKey]; ok {
		return existing, true
	}
	g.fulltextIndex[cacheKey] = built
	return built, true
}

// buildFullTextField scans the string column, tokenizing each labelled
// node's value.
func (g *Snapshot) buildFullTextField(label Label, key PropertyKey) *FullTextField {
	labelNodes := g.labelIndex[label]
	column, ok := g.columns[key]
	if !ok || labelNodes == nil {
		return BuildFullTextField(func(func(uint32, string) bool) {})
	}
	return BuildFullTextField(func(yield func(uint32, string) bool) {
		for node, v := range column.Entries() {
			sid, isStr := v.StrID()
			if !isStr || !labelNodes.Contains(node) {
				continue
			}
			text, ok := g.atoms.Resolve(sid)
			if !ok {
				continue
			}
			if !yield(node, text) {
				return
			}
		}
	})
}

// GeoWithinRadius returns the nodes of label within km great-circle
// distance of (lat, lon), reading the latKey/lonKey f64 properties. The
// per-field index builds lazily and caches.
func (g *Snapshot) GeoWithinRadius(label, latKey, lonKey string, lat, lon, km float64) *nodeset.Set {
	gi, ok := g.geoIndexFor(label, latKey, lonKey)
	if !ok {
		return nodeset.New()
	}
	return gi.WithinRadius(lat, lon, km)
}

// GeoWithinBBox returns the nodes of label whose coordinates fall in the
// lat/lon rectangle; minLon > maxLon crosses the antimeridian.
func (g *Snapshot) GeoWithinBBox(label, latKey, lonKey string, minLat, minLon, maxLat, maxLon float64) *nodeset.Set {
	gi, ok := g.geoIndexFor(label, latKey, lonKey)
	if !ok {
		return nodeset.New()
	}
	return gi.WithinBBox(minLat, minLon, maxLat, maxLon)
}

// GeoKNN returns the k nodes of label nearest (lat, lon), sorted by
// increasing distance, ties by node id.
func (g *Snapshot) GeoKNN(label, latKey, lonKey string, lat, lon float64, k int) []GeoHit {
	gi, ok := g.geoIndexFor(label, latKey, lonKey)
	if !ok {
		return nil
	}
	return gi.KNN(lat, lon, k)
}

func (g *Snapshot) geoIndexFor(label, latKey, lonKey string) (*GeoIndex, bool) {
	labelID, ok := g.Label(label)
	if !ok {
		return nil, false
	}
	latID, ok := g.PropertyKey(latKey)
	if !ok {
		return nil, false
	}
	lonID, ok := g.PropertyKey(lonKey)
	if !ok {
		return nil, false
	}
	cacheKey := geoKey{label: labelID, latKey: latID, lonKey: lonID}
	g.geoMu.Lock()
	cached, ok := g.geoIndex[cacheKey]
	g.geoMu.Unlock()
	if ok {
		return cached, true
	}
	built := g.buildGeoIndex(labelID, latID, lonID)
	g.geoMu.Lock()
	defer g.geoMu.Unlock()
	if existing, ok := g.geoIndex[cacheKey]; ok {
		return existing, true
	}
	g.geoIndex[cacheKey] = built
	return built, true
}

// buildGeoIndex reads each labelled node's lat/lon f64 properties.
func (g *Snapshot) buildGeoIndex(label Label, latKey, lonKey PropertyKey) *GeoIndex {
	labelNodes := g.labelIndex[label]
	latCol, latOK := g.columns[latKey]
	lonCol, lonOK := g.columns[lonKey]
	if !latOK || !lonOK || labelNodes == nil {
		return BuildGeoIndex(func(func(uint32, float64, float64) bool) {})
	}
	return BuildGeoIndex(func(yield func(uint32, float64, float64) bool) {
		for node, latVal := range latCol.Entries() {
			if !labelNodes.Contains(node) {
				continue
			}
			lat, ok := latVal.F64()
			if !ok {
				continue
			}
			lonVal, present := lonCol.Get(node)
			if !present {
				continue
			}
			lon, ok := lonVal.F64()
			if !ok {
				continue
			}
			if !yield(node, lat, lon) {
				return
			}
		}
	})
}
