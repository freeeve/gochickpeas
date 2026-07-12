// Temporal arithmetic for the runtime value type: epoch-millis-backed
// date/datetime values and durations, with civil-calendar decomposition.
// Query-side only -- stored i64-millis columns coerce against these on
// compare. Pure integer math (port of the Rust temporal.rs).
package eval

import "strings"

// MSPerDay is the milliseconds in a civil day.
const MSPerDay int64 = 86_400_000

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
	millis := DaysFromCivil(y, uint32(mo), uint32(d)) * MSPerDay
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
		millis += h*3_600_000 + mi*60_000 + sec*1000 + ms
	}
	return millis, true
}

// ParseISODuration parses an ISO-8601 duration string (PnYnMnWnD[TnHnMnS],
// e.g. 'P100D', 'PT12H', 'P1Y2M3DT4H5M6S') into calendar components:
// months, days, and sub-day milliseconds. Fractional fields and a leading
// sign are not supported; ok is false on a malformed string.
func ParseISODuration(s string) (months, days, ms int64, ok bool) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || (s[0] != 'P' && s[0] != 'p') {
		return 0, 0, 0, false
	}
	inTime := false
	num := int64(-1)
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			if num < 0 {
				num = 0
			}
			num = num*10 + int64(c-'0')
		case c == 'T' || c == 't':
			if num >= 0 || inTime {
				return 0, 0, 0, false
			}
			inTime = true
		default:
			if num < 0 {
				return 0, 0, 0, false
			}
			switch {
			case !inTime && (c == 'Y' || c == 'y'):
				months += num * 12
			case c == 'M' || c == 'm':
				if inTime {
					ms += num * 60_000
				} else {
					months += num
				}
			case !inTime && (c == 'W' || c == 'w'):
				days += num * 7
			case !inTime && (c == 'D' || c == 'd'):
				days += num
			case inTime && (c == 'H' || c == 'h'):
				ms += num * 3_600_000
			case inTime && (c == 'S' || c == 's'):
				ms += num * 1000
			default:
				return 0, 0, 0, false
			}
			num = -1
		}
	}
	// A trailing bare number (or a lone 'P'/'PT') is malformed.
	if num >= 0 {
		return 0, 0, 0, false
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

// ApplyDuration applies a duration (months, days, millis) to an
// epoch-millis temporal, sign = +1 (add) or -1 (subtract). Months are a
// calendar add (day clamped to the target month length, e.g. Jan 31 + 1
// month = Feb 28); days and millis are absolute.
func ApplyDuration(tMillis, months, days, dMillis, sign int64) int64 {
	// A months-free duration is pure tick arithmetic: with months == 0 the
	// civil round-trip below is the identity (no month carry, no day
	// clamp), so the result reduces to a single shifted addition.
	if months == 0 {
		return tMillis + sign*(days*MSPerDay+dMillis)
	}
	baseDays := floorDiv(tMillis, MSPerDay)
	msOfDay := tMillis - baseDays*MSPerDay
	y0, m0, d0 := CivilFromDays(baseDays)
	total := int64(m0) - 1 + sign*months
	y := y0 + floorDiv(total, 12)
	m := uint32(floorMod(total, 12) + 1)
	d := d0
	if dim := DaysInMonth(y, m); d > dim {
		d = dim
	}
	newDays := DaysFromCivil(y, m, d) + sign*days
	return newDays*MSPerDay + msOfDay + sign*dMillis
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
