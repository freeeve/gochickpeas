// Package unorm is a self-contained Unicode normalization implementation
// (UAX #15): NFC, NFD, NFKC, NFKD plus the is-normalized predicate, over
// tables generated from the pinned UCD version (gen/main.go) -- no
// golang.org/x/text dependency. Verified against the full official
// NormalizationTest.txt conformance suite.
package unorm

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// Form is a Unicode normalization form.
type Form uint8

const (
	NFC Form = iota
	NFD
	NFKC
	NFKD
)

// ParseForm resolves a form name (case-insensitive); ok is false for an
// unknown name.
func ParseForm(name string) (Form, bool) {
	switch strings.ToUpper(name) {
	case "NFC":
		return NFC, true
	case "NFD":
		return NFD, true
	case "NFKC":
		return NFKC, true
	case "NFKD":
		return NFKD, true
	}
	return 0, false
}

// Hangul syllable composition constants (UAX #15 section 3.12).
const (
	hangulSBase  = 0xAC00
	hangulLBase  = 0x1100
	hangulVBase  = 0x1161
	hangulTBase  = 0x11A7
	hangulLCount = 19
	hangulVCount = 21
	hangulTCount = 28
	hangulNCount = hangulVCount * hangulTCount
	hangulSCount = hangulLCount * hangulNCount
)

// combiningClass is the canonical combining class of r (0 for the vast
// majority of runes).
func combiningClass(r rune) uint8 {
	lo, hi := 0, len(cccKey)
	for lo < hi {
		mid := (lo + hi) / 2
		if cccKey[mid] < r {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(cccKey) && cccKey[lo] == r {
		return cccVal[lo]
	}
	return 0
}

// decompose looks r up in the requested full-decomposition table; nil
// when r decomposes to itself.
func decompose(r rune, compat bool) []rune {
	key, idx, pool := canonKey[:], canonIdx[:], canonPool[:]
	if compat {
		key, idx, pool = compatKey[:], compatIdx[:], compatPool[:]
	}
	lo, hi := 0, len(key)
	for lo < hi {
		mid := (lo + hi) / 2
		if key[mid] < r {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(key) && key[lo] == r {
		v := idx[lo]
		off, n := v>>8, v&0xff
		return pool[off : off+n]
	}
	return nil
}

// composePair is the primary composite of (a, b), or 0 -- Hangul LV/LVT
// composition handled algorithmically, the rest by table lookup.
func composePair(a, b rune) rune {
	if a >= hangulLBase && a < hangulLBase+hangulLCount &&
		b >= hangulVBase && b < hangulVBase+hangulVCount {
		return hangulSBase + ((a-hangulLBase)*hangulVCount+(b-hangulVBase))*hangulTCount
	}
	if a >= hangulSBase && a < hangulSBase+hangulSCount && (a-hangulSBase)%hangulTCount == 0 &&
		b > hangulTBase && b < hangulTBase+hangulTCount {
		return a + (b - hangulTBase)
	}
	lo, hi := 0, len(compFirst)
	for lo < hi {
		mid := (lo + hi) / 2
		if compFirst[mid] < a || (compFirst[mid] == a && compSecond[mid] < b) {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(compFirst) && compFirst[lo] == a && compSecond[lo] == b {
		return compResult[lo]
	}
	return 0
}

// quickYes reports whether every rune of s is a QC-Yes starter for form
// -- such a string is already normalized and IsNormalized/Normalize can
// answer without allocating. ASCII scans byte-wise.
func quickYes(s string, form Form) bool {
	var lo, hi []rune
	switch form {
	case NFC:
		lo, hi = qcNFCLo[:], qcNFCHi[:]
	case NFD:
		lo, hi = qcNFDLo[:], qcNFDHi[:]
	case NFKC:
		lo, hi = qcNFKCLo[:], qcNFKCHi[:]
	default:
		lo, hi = qcNFKDLo[:], qcNFKDHi[:]
	}
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		j := sort.Search(len(lo), func(k int) bool { return hi[k] >= r })
		if j < len(lo) && lo[j] <= r {
			return false
		}
	}
	return true
}

// appendDecomposed appends r's full decomposition for the form to dst,
// Hangul algorithmically (UAX #15 syllable arithmetic).
func appendDecomposed(dst []rune, r rune, compat bool) []rune {
	if r >= hangulSBase && r < hangulSBase+hangulSCount {
		si := r - hangulSBase
		dst = append(dst, hangulLBase+si/hangulNCount, hangulVBase+(si%hangulNCount)/hangulTCount)
		if t := si % hangulTCount; t != 0 {
			dst = append(dst, hangulTBase+t)
		}
		return dst
	}
	if d := decompose(r, compat); d != nil {
		return append(dst, d...)
	}
	return append(dst, r)
}

// canonicalOrder sorts every maximal run of nonzero-ccc runes stably by
// combining class (the canonical ordering algorithm).
func canonicalOrder(rs []rune) {
	for i := 0; i < len(rs); {
		if combiningClass(rs[i]) == 0 {
			i++
			continue
		}
		j := i
		for j < len(rs) && combiningClass(rs[j]) != 0 {
			j++
		}
		sort.SliceStable(rs[i:j], func(a, b int) bool {
			return combiningClass(rs[i+a]) < combiningClass(rs[i+b])
		})
		i = j
	}
}

// compose runs the canonical composition algorithm in place over a
// canonically-ordered decomposition, returning the shortened slice.
func compose(rs []rune) []rune {
	if len(rs) == 0 {
		return rs
	}
	// out[last] is the most recent starter; blocked tracks the highest
	// combining class seen since it (UAX #15 D115 blocking).
	out := rs[:1]
	last := -1
	if combiningClass(rs[0]) == 0 {
		last = 0
	}
	lastCC := combiningClass(rs[0])
	for _, r := range rs[1:] {
		cc := combiningClass(r)
		if last >= 0 && (lastCC == 0 || lastCC < cc) {
			if c := composePair(out[last], r); c != 0 {
				out[last] = c
				continue
			}
		}
		out = append(out, r)
		if cc == 0 {
			last = len(out) - 1
			lastCC = 0
		} else {
			lastCC = cc
		}
	}
	return out
}

// Normalize returns s in the requested form, returning s itself when the
// quick-check scan proves it already normalized (the common case
// allocates nothing).
func Normalize(s string, form Form) string {
	if quickYes(s, form) {
		return s
	}
	compat := form == NFKC || form == NFKD
	rs := make([]rune, 0, len(s)+8)
	for _, r := range s {
		rs = appendDecomposed(rs, r, compat)
	}
	canonicalOrder(rs)
	if form == NFC || form == NFKC {
		rs = compose(rs)
	}
	return string(rs)
}

// IsNormalized reports whether s is in the requested form: the
// quick-check scan answers the common case without allocating, the rest
// normalize-and-compare.
func IsNormalized(s string, form Form) bool {
	if quickYes(s, form) {
		return true
	}
	return Normalize(s, form) == s
}
