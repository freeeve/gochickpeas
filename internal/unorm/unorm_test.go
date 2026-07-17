package unorm

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestConformance runs the full official NormalizationTest.txt (vendored
// gzipped, same UCD version as the generated tables): for every test row
// the five columns must satisfy the UAX #15 invariants
//
//	c2 == NFC(c1) == NFC(c2) == NFC(c3);  c4 == NFC(c4) == NFC(c5)
//	c3 == NFD(c1) == NFD(c2) == NFD(c3);  c5 == NFD(c4) == NFD(c5)
//	c4 == NFKC(c1..c5)
//	c5 == NFKD(c1..c5)
//
// and every Part 1 character NOT listed must normalize to itself in all
// forms (the suite's completeness clause).
func TestConformance(t *testing.T) {
	f, err := os.Open("testdata/NormalizationTest.txt.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	part1 := map[rune]bool{}
	inPart1 := false
	fails := 0
	check := func(line string, want, got string, inv string) {
		if want != got {
			fails++
			if fails <= 10 {
				t.Errorf("%s: %s: want %q got %q", line, inv, want, got)
			}
		}
	}
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "@Part") {
			inPart1 = strings.HasPrefix(line, "@Part1 ")
			continue
		}
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, ";")
		if len(f) < 5 {
			continue
		}
		var c [5]string
		for i := range 5 {
			var sb strings.Builder
			for _, h := range strings.Fields(f[i]) {
				v, err := strconv.ParseUint(h, 16, 32)
				if err != nil {
					t.Fatalf("bad hex %q in %q", h, line)
				}
				sb.WriteRune(rune(v))
			}
			c[i] = sb.String()
		}
		if inPart1 {
			for _, r := range c[0] {
				part1[r] = true
			}
		}
		id := fmt.Sprintf("%.40s", line)
		for _, src := range c[:3] {
			check(id, c[1], Normalize(src, NFC), "NFC(c1..c3)==c2")
			check(id, c[2], Normalize(src, NFD), "NFD(c1..c3)==c3")
		}
		for _, src := range c[3:] {
			check(id, c[3], Normalize(src, NFC), "NFC(c4..c5)==c4")
			check(id, c[4], Normalize(src, NFD), "NFD(c4..c5)==c5")
		}
		for _, src := range c {
			check(id, c[3], Normalize(src, NFKC), "NFKC(c1..c5)==c4")
			check(id, c[4], Normalize(src, NFKD), "NFKD(c1..c5)==c5")
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(part1) == 0 {
		t.Fatal("Part 1 parsed empty -- test file format drift?")
	}
	// Completeness clause: every assigned code point absent from Part 1
	// must be invariant under all four forms.
	for r := rune(0); r <= 0x10FFFF; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		if part1[r] {
			continue
		}
		s := string(r)
		for _, form := range []Form{NFC, NFD, NFKC, NFKD} {
			if got := Normalize(s, form); got != s {
				fails++
				if fails <= 10 {
					t.Errorf("U+%04X: unlisted rune not invariant under form %d: %q", r, form, got)
				}
			}
		}
	}
	if fails > 10 {
		t.Fatalf("%d total conformance failures (first 10 shown)", fails)
	}
}

// TestIsNormalized pins the predicate against hand cases spanning the
// fast and slow paths (escape-only literals -- combining marks do not
// survive editors).
func TestIsNormalized(t *testing.T) {
	cases := []struct {
		name, s string
		form    Form
		want    bool
	}{
		{"ascii-nfc", "plain ascii", NFC, true},
		{"ascii-nfd", "plain ascii", NFD, true},
		{"precomposed-is-nfc", "caf\u00e9", NFC, true},
		{"precomposed-not-nfd", "caf\u00e9", NFD, false},
		{"decomposed-is-nfd", "cafe\u0301", NFD, true},
		{"decomposed-not-nfc", "cafe\u0301", NFC, false},
		{"fi-ligature-is-nfc", "\ufb01", NFC, true},
		{"fi-ligature-not-nfkc", "\ufb01", NFKC, false},
		{"hangul-syllable-is-nfc", "\uac00", NFC, true},
		{"jamo-not-nfc", "\u1100\u1161", NFC, false},
		{"jamo-is-nfd", "\u1100\u1161", NFD, true},
		{"equal-ccc-order-kept", "a\u0301\u0300", NFD, true},
		{"ccc-ordered", "a\u0323\u0301", NFD, true},
		{"ccc-misordered", "a\u0301\u0323", NFD, false},
		{"angstrom-not-nfc", "\u212b", NFC, false},
	}
	for _, tc := range cases {
		if got := IsNormalized(tc.s, tc.form); got != tc.want {
			t.Errorf("%s: IsNormalized(%q, form %d) = %v, want %v", tc.name, tc.s, tc.form, got, tc.want)
		}
	}
}

// TestNormalizeFastPathAliases pins the zero-alloc contract: an
// already-normalized ASCII string comes back as the SAME string value
// with no allocation.
func TestNormalizeFastPathAliases(t *testing.T) {
	s := "already normalized ascii"
	allocs := testing.AllocsPerRun(100, func() {
		if Normalize(s, NFC) != s {
			t.Fatal("changed")
		}
	})
	if allocs != 0 {
		t.Fatalf("fast path allocated %v/op", allocs)
	}
}

func TestParseForm(t *testing.T) {
	for name, want := range map[string]Form{"nfc": NFC, "NFD": NFD, "NfKc": NFKC, "NFKD": NFKD} {
		got, ok := ParseForm(name)
		if !ok || got != want {
			t.Fatalf("ParseForm(%q) = %v, %v", name, got, ok)
		}
	}
	if _, ok := ParseForm("NFX"); ok {
		t.Fatal("NFX parsed")
	}
}
