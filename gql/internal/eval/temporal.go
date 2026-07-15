// Temporal arithmetic for the runtime value type: epoch-millis-backed
// date/datetime values and durations, with civil-calendar decomposition.
// Query-side only -- stored i64-millis columns coerce against these on
// compare. Pure integer math (port of the Rust temporal.rs).
package eval

import (
	"fmt"
	"math"
	"strings"

	"github.com/freeeve/gochickpeas/gql/value"
)

// MSPerDay is the milliseconds in a civil day.
const MSPerDay int64 = 86_400_000

// maxCivilYear bounds a civil year to the epoch-millis representable range
// (i64 milliseconds span roughly +/-292 million years). Beyond it the era
// term in DaysFromCivil (era*146_097) or the later millisecond scaling
// overflows silently, so temporal construction is bounded here first and
// yields Null rather than a wrapped nonsense instant.
const maxCivilYear int64 = 300_000_000

// addChk / mulChk are checked int64 add / multiply (comma-ok), the temporal
// path's guard against silent wrap -- overflow becomes Null, matching the
// engine's integer-overflow policy.
func addChk(a, b int64) (int64, bool) {
	c := a + b
	return c, (c > a) == (b > 0) || b == 0
}

func mulChk(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if (a == math.MinInt64 && b == -1) || (b == math.MinInt64 && a == -1) {
		return 0, false
	}
	c := a * b
	return c, c/b == a
}

// civilMillis converts a civil (year, month, day) plus a sub-day millisecond
// offset to epoch millis, ok=false when the year is out of range or the
// scaling overflows -- so a constructed temporal never wraps.
func civilMillis(y int64, m, d uint32, msOfDay int64) (int64, bool) {
	if y < -maxCivilYear || y > maxCivilYear {
		return 0, false
	}
	ms, ok := mulChk(DaysFromCivil(y, m, d), MSPerDay)
	if !ok {
		return 0, false
	}
	return addChk(ms, msOfDay)
}

// DaysFromCivil is days since 1970-01-01 for a civil (year, month, day) --
// Howard Hinnant's algorithm.
func DaysFromCivil(y int64, m, d uint32) int64 {
	if m <= 2 {
		y--
	}
	era := y
	if y < 0 {
		era = y - 399
	}
	era /= 400
	yoe := y - era*400
	mp := int64(m+9) % 12
	doy := (153*mp+2)/5 + int64(d) - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146_097 + doe - 719_468
}

// CivilFromDays is the inverse: civil (year, month, day) from days since
// 1970-01-01.
func CivilFromDays(z int64) (y int64, m, d uint32) {
	z += 719_468
	era := z
	if z < 0 {
		era = z - 146_096
	}
	era /= 146_097
	doe := z - era*146_097
	yoe := (doe - doe/1460 + doe/36524 - doe/146_096) / 365
	y = yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d = uint32(doy - (153*mp+2)/5 + 1)
	if mp < 10 {
		m = uint32(mp + 3)
	} else {
		m = uint32(mp - 9)
	}
	if m <= 2 {
		y++
	}
	return y, m, d
}

// DaysInMonth is the day count of (year, month), accounting for leap years
// (used to clamp a calendar + duration).
func DaysInMonth(y int64, m uint32) uint32 {
	switch m {
	case 1, 3, 5, 7, 8, 10, 12:
		return 31
	case 4, 6, 9, 11:
		return 30
	case 2:
		if (y%4 == 0 && y%100 != 0) || y%400 == 0 {
			return 29
		}
		return 28
	}
	return 30
}

// ParseISO parses an ISO-ish temporal string into epoch milliseconds
// (UTC): YYYY-MM-DD, optionally THH:MM[:SS[.mmm]] and a trailing Z/offset
// (the offset is accepted but treated as UTC -- no real zone handling).
// ok is false on a malformed string.
func ParseISO(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	datePart := s
	timePart := ""
	if i := strings.IndexAny(s, "T "); i >= 0 {
		datePart, timePart = s[:i], s[i+1:]
	}
	dp := strings.Split(datePart, "-")
	if len(dp) < 3 {
		return 0, false
	}
	y, ok1 := parseI64(dp[0])
	mo, ok2 := parseI64(dp[1])
	d, ok3 := parseI64(dp[2])
	if !ok1 || !ok2 || !ok3 || mo < 1 || mo > 12 || d < 1 || d > 31 {
		return 0, false
	}
	var msOfDay int64
	if timePart != "" {
		t := strings.TrimSuffix(timePart, "Z")
		if i := strings.IndexByte(t, '+'); i >= 0 {
			t = t[:i]
		}
		ti := strings.Split(t, ":")
		h, ok := parseI64(ti[0])
		if !ok {
			return 0, false
		}
		var mi, sec, ms int64
		if len(ti) >= 2 {
			if mi, ok = parseI64(ti[1]); !ok {
				return 0, false
			}
		}
		if len(ti) >= 3 {
			secStr, frac, hasFrac := strings.Cut(ti[2], ".")
			if sec, ok = parseI64(secStr); !ok {
				return 0, false
			}
			if hasFrac {
				f := frac
				if len(f) > 3 {
					f = f[:3]
				}
				for len(f) < 3 {
					f += "0"
				}
				// A non-numeric fraction reads as 0, matching the Rust
				// parser's unwrap_or(0).
				ms, _ = parseI64(f)
			}
		}
		msOfDay = h*3_600_000 + mi*60_000 + sec*1000 + ms
	}
	// An absurd year overflows the era term / millisecond scaling; bound it
	// so a constructed instant never silently wraps.
	return civilMillis(y, uint32(mo), uint32(d), msOfDay)
}

// ISOString renders an epoch-millis temporal as ISO-8601, the inverse of
// ParseISO: 'YYYY-MM-DD' for a Date; 'YYYY-MM-DDTHH:MM:SS' for the datetime
// kinds, with '.mmm' appended only when the sub-second remainder is
// non-zero. This is the single temporal string formatter -- any other
// surface that stringifies a temporal should delegate here so the forms
// cannot drift.
func ISOString(millis int64, kind value.TemporalKind) string {
	days := floorDiv(millis, MSPerDay)
	msOfDay := millis - days*MSPerDay
	y, mo, d := CivilFromDays(days)
	if kind == value.Date {
		return fmt.Sprintf("%04d-%02d-%02d", y, mo, d)
	}
	h := msOfDay / 3_600_000
	mi := (msOfDay / 60_000) % 60
	s := (msOfDay / 1000) % 60
	if frac := msOfDay % 1000; frac != 0 {
		return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d.%03d", y, mo, d, h, mi, s, frac)
	}
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d", y, mo, d, h, mi, s)
}

// ParseISODuration parses an ISO-8601 duration string (PnYnMnWnD[TnHnMnS],
// e.g. 'P100D', 'PT12H', 'P1Y2M3DT4H5M6S') into calendar components:
// months, days, and sub-day milliseconds. Fractional fields and a leading
// sign are not supported; ok is false on a malformed string.
func ParseISODuration(s string) (months, days, ms int64, ok bool) {
	s = strings.TrimSpace(s)
	// A leading sign negates the whole duration ('-P1D').
	neg := false
	if len(s) > 0 && (s[0] == '-' || s[0] == '+') {
		neg = s[0] == '-'
		s = s[1:]
	}
	if len(s) < 2 || (s[0] != 'P' && s[0] != 'p') {
		return 0, 0, 0, false
	}
	inTime := false
	sawComponent := false
	num := int64(-1)
	fracMs := int64(-1) // parsed fractional-seconds millis, -1 = none
	// A field that overflows its unit conversion or the running total is a
	// wrapped nonsense duration; decline it (Null) rather than build one.
	add := func(acc, delta int64) (int64, bool) { return addChk(acc, delta) }
	scale := func(n, k int64) (int64, bool) { return mulChk(n, k) }
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			if num < 0 {
				num = 0
			}
			t, ok := scale(num, 10)
			if !ok {
				return 0, 0, 0, false
			}
			if num, ok = add(t, int64(c-'0')); !ok {
				return 0, 0, 0, false
			}
		case c == 'T' || c == 't':
			if num >= 0 || inTime {
				return 0, 0, 0, false
			}
			inTime = true
		case c == '.':
			// A fraction is only valid on the seconds field: digits, then
			// 'S'. Millisecond precision (first three digits, zero-padded).
			if !inTime || num < 0 || fracMs >= 0 {
				return 0, 0, 0, false
			}
			j := i + 1
			f, digits := int64(0), 0
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				if digits < 3 {
					f = f*10 + int64(s[j]-'0')
					digits++
				}
				j++
			}
			if digits == 0 || j >= len(s) || (s[j] != 'S' && s[j] != 's') {
				return 0, 0, 0, false
			}
			for digits < 3 {
				f *= 10
				digits++
			}
			fracMs = f
			i = j - 1 // the loop's i++ lands on the 'S'
		default:
			if num < 0 {
				return 0, 0, 0, false
			}
			var okc bool
			switch {
			case !inTime && (c == 'Y' || c == 'y'):
				var t int64
				if t, okc = scale(num, 12); okc {
					months, okc = add(months, t)
				}
			case c == 'M' || c == 'm':
				if inTime {
					var t int64
					if t, okc = scale(num, 60_000); okc {
						ms, okc = add(ms, t)
					}
				} else {
					months, okc = add(months, num)
				}
			case !inTime && (c == 'W' || c == 'w'):
				var t int64
				if t, okc = scale(num, 7); okc {
					days, okc = add(days, t)
				}
			case !inTime && (c == 'D' || c == 'd'):
				days, okc = add(days, num)
			case inTime && (c == 'H' || c == 'h'):
				var t int64
				if t, okc = scale(num, 3_600_000); okc {
					ms, okc = add(ms, t)
				}
			case inTime && (c == 'S' || c == 's'):
				var t int64
				if t, okc = scale(num, 1000); okc {
					if fracMs >= 0 {
						t, okc = add(t, fracMs)
						fracMs = -1
					}
					if okc {
						ms, okc = add(ms, t)
					}
				}
			default:
				return 0, 0, 0, false
			}
			if !okc {
				return 0, 0, 0, false
			}
			sawComponent = true
			num = -1
		}
	}
	// A trailing bare number, an unconsumed fraction, or a designator-only
	// string ('P', 'PT') is malformed -- never a partial parse.
	if num >= 0 || fracMs >= 0 || !sawComponent {
		return 0, 0, 0, false
	}
	if neg {
		months, days, ms = -months, -days, -ms
	}
	return months, days, ms, true
}

// Component is a temporal component accessor (.year/.month/.day/.hour/...
// /.epochMillis) on an epoch-millis temporal; ok is false for an unknown
// component (the caller yields Null).
func Component(millis int64, key string) (int64, bool) {
	days := floorDiv(millis, MSPerDay)
	msOfDay := millis - days*MSPerDay
	y, mo, d := CivilFromDays(days)
	switch key {
	case "year":
		return y, true
	case "month":
		return int64(mo), true
	case "day":
		return int64(d), true
	case "hour":
		return msOfDay / 3_600_000, true
	case "minute":
		return (msOfDay / 60_000) % 60, true
	case "second":
		return (msOfDay / 1000) % 60, true
	case "millisecond":
		return msOfDay % 1000, true
	case "epochMillis":
		return millis, true
	case "epochSeconds":
		return floorDiv(millis, 1000), true
	}
	return 0, false
}

// DurationComponent is a duration component accessor on a (months, days,
// millis) duration, with the groups independent (Neo4j's convention): the
// months group answers years/quarters/months (months is the group TOTAL,
// so duration({years: 1}).months = 12), the days group weeks/days, and
// the millis group hours/minutes/seconds/milliseconds (each the group
// total at that unit, so PT2H reads .minutes = 120). ok=false for an
// unknown component.
func DurationComponent(months, days, millis int64, key string) (int64, bool) {
	switch key {
	case "years":
		return months / 12, true
	case "quarters":
		return months / 3, true
	case "months":
		return months, true
	case "weeks":
		return days / 7, true
	case "days":
		return days, true
	case "hours":
		return millis / 3_600_000, true
	case "minutes":
		return millis / 60_000, true
	case "seconds":
		return millis / 1000, true
	case "milliseconds":
		return millis, true
	}
	return 0, false
}

// ApplyDuration applies a duration (months, days, millis) to an
// epoch-millis temporal, sign = +1 (add) or -1 (subtract). Months are a
// calendar add (day clamped to the target month length, e.g. Jan 31 + 1
// month = Feb 28); days and millis are absolute. Every step is checked:
// ok=false on overflow, so the caller yields Null rather than a wrapped
// nonsense instant. Note the boundary -- shifting a recent instant by
// i64-scale milliseconds to a very negative-but-representable value is exact
// and stays ok; only a genuine overflow declines.
func ApplyDuration(tMillis, months, days, dMillis, sign int64) (int64, bool) {
	// A months-free duration is pure tick arithmetic: with months == 0 the
	// civil round-trip below is the identity (no month carry, no day
	// clamp), so the result reduces to a single shifted addition.
	if months == 0 {
		tick, ok := mulChk(days, MSPerDay)
		if !ok {
			return 0, false
		}
		if tick, ok = addChk(tick, dMillis); !ok {
			return 0, false
		}
		if tick, ok = mulChk(sign, tick); !ok {
			return 0, false
		}
		return addChk(tMillis, tick)
	}
	baseDays := floorDiv(tMillis, MSPerDay)
	msOfDay := tMillis - baseDays*MSPerDay
	y0, m0, d0 := CivilFromDays(baseDays)
	sm, ok := mulChk(sign, months)
	if !ok {
		return 0, false
	}
	total, ok := addChk(int64(m0)-1, sm)
	if !ok {
		return 0, false
	}
	// y0 is bounded (a representable instant) and floorDiv(total, 12) fits,
	// so this sum cannot overflow; the range guard below rejects an absurd
	// year before DaysFromCivil's era term can overflow.
	y := y0 + floorDiv(total, 12)
	if y < -maxCivilYear || y > maxCivilYear {
		return 0, false
	}
	m := uint32(floorMod(total, 12) + 1)
	d := d0
	if dim := DaysInMonth(y, m); d > dim {
		d = dim
	}
	sd, ok := mulChk(sign, days)
	if !ok {
		return 0, false
	}
	newDays, ok := addChk(DaysFromCivil(y, m, d), sd)
	if !ok {
		return 0, false
	}
	r, ok := mulChk(newDays, MSPerDay)
	if !ok {
		return 0, false
	}
	if r, ok = addChk(r, msOfDay); !ok {
		return 0, false
	}
	sdm, ok := mulChk(sign, dMillis)
	if !ok {
		return 0, false
	}
	return addChk(r, sdm)
}

// floorDiv is Euclidean-style division rounding toward negative infinity
// (Rust's div_euclid for positive divisors).
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// floorMod is the non-negative remainder matching floorDiv.
func floorMod(a, b int64) int64 {
	return a - floorDiv(a, b)*b
}

// parseI64 parses a decimal integer with optional sign and surrounding
// spaces, comma-ok.
func parseI64(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	neg := false
	if s[0] == '+' || s[0] == '-' {
		neg = s[0] == '-'
		s = s[1:]
		if s == "" {
			return 0, false
		}
	}
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	if neg {
		n = -n
	}
	return n, true
}
