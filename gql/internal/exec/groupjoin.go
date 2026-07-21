// Group-join stage runner: the decorrelated OPTIONAL-MATCH aggregate
// join. The standalone inner plan runs once (lazily, on the first outer
// row) into a key -> aggregate-values table; each outer row binds its
// synthetic aggregate slots by correlation-key lookup, with the
// aggregates' empty-group identities as the fill. The segment projection
// re-aggregates the synthetic columns, so the emitted rows reproduce the
// nested left-join answer exactly.
package exec

import (
	"github.com/freeeve/gochickpeas/flatset"
	"github.com/freeeve/gochickpeas/gql/internal/eval"
	"github.com/freeeve/gochickpeas/gql/internal/plan"
	"github.com/freeeve/gochickpeas/gql/value"
)

type groupJoinSink struct {
	ctx *eval.Ctx
	gj  *plan.GroupJoinStage
	buf []value.Value
	// table maps an encoded correlation key to its group's aggregate
	// values through the interned-key probe table -- the map[string] form
	// paid one heap string per group on build.
	table  flatset.ByteMap
	groups [][]value.Value
	built  bool
	keyBuf []byte
	next   rowSink
	count  *uint64
}

func (s *groupJoinSink) push(row []value.Value) bool {
	if !s.built {
		nk := len(s.gj.KeySlots)
		for _, sr := range runSubplan(s.ctx, s.gj.Sub, nil) {
			k := s.keyBuf[:0]
			for i := range nk {
				k = value.AppendKey(k, sr[i])
			}
			s.keyBuf = k
			vals := sr[nk:]
			i := s.table.GetOrCreate(k, func() int {
				s.groups = append(s.groups, vals)
				return len(s.groups) - 1
			})
			s.groups[i] = vals
		}
		s.built = true
	}
	k := s.keyBuf[:0]
	for _, ks := range s.gj.KeySlots {
		k = value.AppendKey(k, row[ks])
	}
	s.keyBuf = k
	copy(s.buf, row)
	if i, ok := s.table.Get(k); ok {
		vals := s.groups[i]
		for i, o := range s.gj.OutSlots {
			s.buf[o] = vals[i]
		}
	} else {
		for i, o := range s.gj.OutSlots {
			if s.gj.Fills[i] == plan.FillZero {
				s.buf[o] = value.Int(0)
			} else {
				s.buf[o] = value.Null()
			}
		}
	}
	if s.count != nil {
		*s.count++
	}
	return s.next.push(s.buf)
}

func (s *groupJoinSink) close() { s.next.close() }
