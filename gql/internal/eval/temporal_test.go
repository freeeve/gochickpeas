// Temporal math unit tests: civil-calendar round trips, ISO parsing
// forms, component reads (including pre-1970 floor semantics), and
// calendar-clamped duration application.
package eval

import (
	"math"
	"testing"
)

func TestCivilRoundTrip(t *testing.T) {
	cases := []struct {
		y int64
		m uint32
		d uint32
	}{
		{1970, 1, 1}, {2000, 2, 29}, {2020, 12, 31}, {1969, 12, 31},
		{1900, 3, 1}, {2400, 2, 29}, {1, 1, 1}, {-1, 6, 15},
	}
	for _, c := range cases {
		days := DaysFromCivil(c.y, c.m, c.d)
		y, m, d := CivilFromDays(days)
		if y != c.y || m != c.m || d != c.d {
			t.Fatalf("round trip %v -> %d -> (%d,%d,%d)", c, days, y, m, d)
		}
	}
	if DaysFromCivil(1970, 1, 1) != 0 {
		t.Fatal("epoch day is 0")
	}
	if DaysFromCivil(1970, 1, 2) != 1 {
		t.Fatal("epoch day 2 is 1")
	}
}

func TestDaysInMonth(t *testing.T) {
	if DaysInMonth(2020, 2) != 29 || DaysInMonth(2021, 2) != 28 {
		t.Fatal("leap February")
	}
	if DaysInMonth(1900, 2) != 28 || DaysInMonth(2000, 2) != 29 {
		t.Fatal("century leap rules")
	}
	if DaysInMonth(2021, 4) != 30 || DaysInMonth(2021, 7) != 31 {
		t.Fatal("month lengths")
	}
}

func TestParseISOForms(t *testing.T) {
	ms, ok := ParseISO("1970-01-02")
	if !ok || ms != MSPerDay {
		t.Fatalf("date-only = (%d, %v)", ms, ok)
	}
	ms, ok = ParseISO("1970-01-01T01:02:03.500Z")
	if !ok || ms != 3_600_000+2*60_000+3_000+500 {
		t.Fatalf("full = (%d, %v)", ms, ok)
	}
	// Space separator, minutes-only, fraction truncation to millis, +offset
	// treated as UTC.
	if ms, ok = ParseISO("1970-01-01 01:30"); !ok || ms != 3_600_000+30*60_000 {
		t.Fatalf("hh:mm = (%d, %v)", ms, ok)
	}
	if ms, ok = ParseISO("1970-01-01T00:00:01.123456"); !ok || ms != 1123 {
		t.Fatalf("fraction = (%d, %v)", ms, ok)
	}
	if ms, ok = ParseISO("1970-01-01T00:00:01.5"); !ok || ms != 1500 {
		t.Fatalf("short fraction right-pads = (%d, %v)", ms, ok)
	}
	if ms, ok = ParseISO("1970-01-01T02:00:00+05:00"); !ok || ms != 2*3_600_000 {
		t.Fatalf("offset treated as UTC = (%d, %v)", ms, ok)
	}
	for _, bad := range []string{"nope", "2020-13-01", "2020-00-01", "2020-01-32", "2020-01", "2020-01-01Tzz:00"} {
		if _, ok := ParseISO(bad); ok {
			t.Fatalf("%q should not parse", bad)
		}
	}
}

func TestComponents(t *testing.T) {
	ms, _ := ParseISO("2020-03-15T10:30:45.250")
	want := map[string]int64{
		"year": 2020, "month": 3, "day": 15, "hour": 10, "minute": 30,
		"second": 45, "millisecond": 250, "epochMillis": ms,
		"epochSeconds": ms / 1000,
	}
	for k, w := range want {
		if got, ok := Component(ms, k); !ok || got != w {
			t.Fatalf("%s = (%d, %v), want %d", k, got, ok, w)
		}
	}
	if _, ok := Component(ms, "nope"); ok {
		t.Fatal("unknown component")
	}
	// Pre-1970: floor division keeps components civil.
	neg, _ := ParseISO("1969-12-31T23:00:00")
	if y, _ := Component(neg, "year"); y != 1969 {
		t.Fatalf("pre-epoch year = %d", y)
	}
	if h, _ := Component(neg, "hour"); h != 23 {
		t.Fatalf("pre-epoch hour = %d", h)
	}
	if s, _ := Component(-1500, "epochSeconds"); s != -2 {
		t.Fatalf("epochSeconds floors toward -inf: %d", s)
	}
}

func TestApplyDurationClamps(t *testing.T) {
	jan31, _ := ParseISO("2020-01-31")
	feb, ok := ApplyDuration(jan31, 1, 0, 0, 1)
	if !ok {
		t.Fatal("Jan 31 + 1 month overflowed unexpectedly")
	}
	if d, _ := Component(feb, "day"); d != 29 {
		t.Fatalf("Jan 31 + 1 month (leap) = day %d, want 29", d)
	}
	if m, _ := Component(feb, "month"); m != 2 {
		t.Fatalf("month = %d", m)
	}
	jan31ny, _ := ParseISO("2021-01-31")
	feb28, _ := ApplyDuration(jan31ny, 1, 0, 0, 1)
	if d, _ := Component(feb28, "day"); d != 28 {
		t.Fatalf("Jan 31 + 1 month (non-leap) = day %d, want 28", d)
	}
	// Subtracting months crosses a year boundary.
	mar, _ := ParseISO("2020-03-15")
	back, _ := ApplyDuration(mar, 4, 0, 0, -1)
	if y, _ := Component(back, "year"); y != 2019 {
		t.Fatalf("year = %d", y)
	}
	if m, _ := Component(back, "month"); m != 11 {
		t.Fatalf("month = %d", m)
	}
	// Days and millis are absolute; time-of-day is preserved across the
	// calendar add.
	noon, _ := ParseISO("2020-01-31T12:00:00")
	shifted, _ := ApplyDuration(noon, 1, 2, 500, 1)
	if h, _ := Component(shifted, "hour"); h != 12 {
		t.Fatalf("hour preserved = %d", h)
	}
}

// A genuine overflow in either duration path declines to ok=false so the
// caller yields Null, never a silently wrapped instant (Go wraps in every
// build, so the value -- not merely "no panic" -- is what a test must pin).
func TestApplyDurationOverflowIsDeclined(t *testing.T) {
	base, _ := ParseISO("2020-01-01")
	cases := []struct {
		name                        string
		months, days, dMillis, sign int64
	}{
		{"months-free days*MSPerDay overflows", 0, math.MaxInt64, 0, 1},
		{"months-free millis add overflows", 0, 0, math.MaxInt64, 1},
		{"calendar month count overflows", math.MaxInt64, 0, 0, 1},
		{"calendar year out of range", math.MaxInt64 / 20, 0, 0, 1},
		{"calendar day scaling overflows", 1, math.MaxInt64, 0, 1},
	}
	for _, c := range cases {
		if got, ok := ApplyDuration(base, c.months, c.days, c.dMillis, c.sign); ok {
			t.Fatalf("%s: got %d ok=true, want declined", c.name, got)
		}
	}
}

// The boundary the rcp twin flagged: subtracting an i64-scale millisecond
// duration from a recent instant reaches a very negative but representable
// value, so it stays a value, not Null. Only true overflow declines.
func TestApplyDurationRepresentableBoundaryStays(t *testing.T) {
	recent, _ := ParseISO("2020-01-01") // ~1.577e12 ms, well above MinInt64+MaxInt64
	got, ok := ApplyDuration(recent, 0, 0, math.MaxInt64, -1)
	if !ok {
		t.Fatal("recent - MaxInt64 ms is representable but was declined")
	}
	if want := recent - math.MaxInt64; got != want {
		t.Fatalf("boundary value = %d, want %d", got, want)
	}
}

// Duration construction (map and ISO forms) overflows at build time, before
// any temporal is touched.
func TestDurationConstructionOverflow(t *testing.T) {
	// ISO: a unit conversion (years*12) and a bare-number parse both overflow.
	if _, _, _, ok := ParseISODuration("P9223372036854775807Y"); ok {
		t.Fatal("P<huge>Y should decline (years*12 overflows)")
	}
	if _, _, _, ok := ParseISODuration("PT9999999999999999999S"); ok {
		t.Fatal("a number wider than i64 should decline")
	}
	// A representable ISO duration still parses.
	if mo, d, ms, ok := ParseISODuration("P1Y2M3DT4H"); !ok || mo != 14 || d != 3 || ms != 4*3_600_000 {
		t.Fatalf("P1Y2M3DT4H = (mo %d, d %d, ms %d, ok %v)", mo, d, ms, ok)
	}
}

// An absurd civil year overflows the era term inside DaysFromCivil before a
// naive check would see it; civilMillis bounds it first so ParseISO declines.
func TestParseISOAbsurdYearDeclines(t *testing.T) {
	if ms, ok := ParseISO("300000001-01-01"); ok {
		t.Fatalf("year past the representable bound should decline, got %d", ms)
	}
	if _, ok := ParseISO("2020-06-15"); !ok {
		t.Fatal("an ordinary date must still parse")
	}
}
