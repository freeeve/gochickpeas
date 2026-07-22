package eval

import "testing"

// TestDurationComponent covers the (months, days, millis) duration accessor,
// including the independent-group totals from the doc (a years-only duration
// reads .months as the month total; a PT2H duration reads .minutes as 120).
func TestDurationComponent(t *testing.T) {
	// duration({years: 1}) -> 12 months: the months group answers
	// years/quarters/months off the same total.
	for key, want := range map[string]int64{"years": 1, "quarters": 4, "months": 12} {
		if got, ok := DurationComponent(12, 0, 0, key); !ok || got != want {
			t.Fatalf("{months:12}.%s = (%d,%v), want %d", key, got, ok, want)
		}
	}
	// duration({weeks: 2}) -> 14 days.
	for key, want := range map[string]int64{"weeks": 2, "days": 14} {
		if got, ok := DurationComponent(0, 14, 0, key); !ok || got != want {
			t.Fatalf("{days:14}.%s = (%d,%v), want %d", key, got, ok, want)
		}
	}
	// PT2H -> 7_200_000 ms: the millis group totals at each unit.
	for key, want := range map[string]int64{
		"hours": 2, "minutes": 120, "seconds": 7200, "milliseconds": 7_200_000,
	} {
		if got, ok := DurationComponent(0, 0, 7_200_000, key); !ok || got != want {
			t.Fatalf("PT2H.%s = (%d,%v), want %d", key, got, ok, want)
		}
	}
	if _, ok := DurationComponent(1, 1, 1, "fortnights"); ok {
		t.Fatal("unknown duration component should not resolve")
	}
}
