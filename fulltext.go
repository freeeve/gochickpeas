// Full-text index over string node properties: boolean retrieval + BM25.
// Tokenization is lowercase + split on non-alphanumeric runs (Unicode-
// aware); stopword removal and stemming are deferred, matching the Rust v1.

package chickpeas

import (
	"sort"
	"strings"
	"unicode"

	"github.com/RoaringBitmap/roaring/v2"
	"github.com/freeeve/gochickpeas/nodeset"
)

const (
	bm25K1 = 1.2  // term-frequency saturation
	bm25B  = 0.75 // length normalization
)

type termPostings struct {
	docs *roaring.Bitmap
	tf   map[uint32]uint32
}

// FullTextField is the inverted index for a single (label, property) text
// field. Build directly or query through Snapshot.FullTextSearch, which
// builds and caches per field lazily.
type FullTextField struct {
	postings map[string]*termPostings
	docLen   map[uint32]uint32
	totalLen uint64
}

// Tokenize splits text into lowercased alphanumeric runs -- the shared
// tokenizer of the build and query paths.
func Tokenize(text string) []string {
	runs := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	for i, run := range runs {
		runs[i] = strings.ToLower(run)
	}
	return runs
}

// BuildFullTextField indexes already label-filtered (node, text) documents.
func BuildFullTextField(docs func(yield func(node uint32, text string) bool)) *FullTextField {
	f := &FullTextField{postings: map[string]*termPostings{}, docLen: map[uint32]uint32{}}
	docs(func(node uint32, text string) bool {
		length := uint32(0)
		for _, token := range Tokenize(text) {
			length++
			tp, ok := f.postings[token]
			if !ok {
				tp = &termPostings{docs: roaring.New(), tf: map[uint32]uint32{}}
				f.postings[token] = tp
			}
			tp.docs.Add(node)
			tp.tf[node]++
		}
		if length > 0 {
			f.docLen[node] += length
			f.totalLen += uint64(length)
		}
		return true
	})
	return f
}

// Query returns the nodes whose text contains EVERY token in query
// (boolean AND). An empty query, or any token absent from the field,
// yields the empty set.
func (f *FullTextField) Query(query string) *nodeset.Set {
	var acc *roaring.Bitmap
	tokens := Tokenize(query)
	if len(tokens) == 0 {
		return nodeset.New()
	}
	for _, token := range tokens {
		tp, ok := f.postings[token]
		if !ok {
			return nodeset.New()
		}
		if acc == nil {
			acc = tp.docs.Clone()
		} else {
			acc.And(tp.docs)
		}
		if acc.IsEmpty() {
			return nodeset.New()
		}
	}
	return nodeset.FromBitmap(acc)
}

// RankedHit is one QueryRanked result.
type RankedHit struct {
	Node  uint32
	Score float32
}

// QueryRanked returns the top k nodes by BM25 relevance (disjunctive: a
// node scores for every query token it contains), sorted by score
// descending, ties by ascending node id.
func (f *FullTextField) QueryRanked(query string, k int) []RankedHit {
	n := len(f.docLen)
	if n == 0 || k <= 0 {
		return nil
	}
	avgdl := float32(f.totalLen) / float32(n)
	scores := map[uint32]float32{}
	for _, token := range Tokenize(query) {
		tp, ok := f.postings[token]
		if !ok {
			continue
		}
		df := float32(tp.docs.GetCardinality())
		idf := ln32(1 + (float32(n)-df+0.5)/(df+0.5))
		for node, rawTF := range tp.tf {
			tf := float32(rawTF)
			dl := float32(f.docLen[node])
			denom := tf + bm25K1*(1-bm25B+bm25B*dl/avgdl)
			scores[node] += idf * (tf * (bm25K1 + 1)) / denom
		}
	}
	ranked := make([]RankedHit, 0, len(scores))
	for node, score := range scores {
		ranked = append(ranked, RankedHit{Node: node, Score: score})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		return ranked[i].Node < ranked[j].Node
	})
	return ranked[:min(k, len(ranked))]
}

// TermCount is the number of distinct indexed tokens.
func (f *FullTextField) TermCount() int { return len(f.postings) }

// DocCount is the number of indexed documents (nodes with non-empty text).
func (f *FullTextField) DocCount() int { return len(f.docLen) }
