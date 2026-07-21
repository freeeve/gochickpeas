// Temporal component access and duration arithmetic: reading a named
// component off an epoch-millis instant or a (months, days, millis)
// duration, applying a duration to an instant (checked calendar add), and
// the Euclidean integer-math helpers they share. Split from temporal.go,
// which holds the ISO parsing and formatting.
package eval

import "strings"

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
