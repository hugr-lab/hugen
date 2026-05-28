package runner

import (
	"testing"
	"time"
)

func TestEveryNextAdvances(t *testing.T) {
	t.Parallel()
	s := Every(2 * time.Second)
	now := time.Unix(1700000000, 0)
	got := s.Next(now)
	want := now.Add(2 * time.Second)
	if !got.Equal(want) {
		t.Fatalf("Every.Next mismatch: got %v want %v", got, want)
	}
}

func TestEveryZeroDurationReturnsZero(t *testing.T) {
	t.Parallel()
	s := Every(0)
	if got := s.Next(time.Now()); !got.IsZero() {
		t.Fatalf("Every(0) should return zero time, got %v", got)
	}
}

func TestEveryNegativeDurationReturnsZero(t *testing.T) {
	t.Parallel()
	s := Every(-time.Second)
	if got := s.Next(time.Now()); !got.IsZero() {
		t.Fatalf("Every(-) should return zero time, got %v", got)
	}
}

func TestOnceFiresThenZero(t *testing.T) {
	t.Parallel()
	target := time.Unix(1700000000, 0)
	s := Once(target)

	if got := s.Next(target.Add(-time.Second)); !got.Equal(target) {
		t.Fatalf("Once.Next before target: got %v want %v", got, target)
	}
	// Boundary: Once(at).Next(at) MUST return at — otherwise
	// Register(Once(time.Now())) would race the clock and silently
	// install a dead registration with nextFireAt=zero.
	if got := s.Next(target); !got.Equal(target) {
		t.Fatalf("Once.Next at target should return target, got %v", got)
	}
	if got := s.Next(target.Add(time.Second)); !got.IsZero() {
		t.Fatalf("Once.Next after target should be zero, got %v", got)
	}
}

func TestCronInvalidExprIsConstructorError(t *testing.T) {
	t.Parallel()
	for _, expr := range []string{"", "not a cron", "* * * *", "60 * * * *", "0 9 * * 1 extra"} {
		if _, err := Cron(expr, time.UTC); err == nil {
			t.Fatalf("Cron(%q) expected error, got nil", expr)
		}
	}
}

func TestCronDayOfWeek(t *testing.T) {
	t.Parallel()
	// "0 9 * * 1" = Monday 09:00. 2024-01-01 is a Monday.
	s, err := Cron("0 9 * * 1", time.UTC)
	if err != nil {
		t.Fatalf("Cron: %v", err)
	}
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	want := time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC)
	if got := s.Next(from); !got.Equal(want) {
		t.Fatalf("Cron DOW first fire: got %v want %v", got, want)
	}
	// robfig Next is strictly-after: from the planned instant itself
	// the next fire is the FOLLOWING Monday, not the same one. This
	// is what makes recurring cron advance instead of self-repeating.
	wantNext := time.Date(2024, 1, 8, 9, 0, 0, 0, time.UTC)
	if got := s.Next(want); !got.Equal(wantNext) {
		t.Fatalf("Cron DOW exclusive boundary: got %v want %v", got, wantNext)
	}
}

func TestCronDayOfMonth(t *testing.T) {
	t.Parallel()
	// "0 0 1 * *" = first of every month at midnight.
	s, err := Cron("0 0 1 * *", time.UTC)
	if err != nil {
		t.Fatalf("Cron: %v", err)
	}
	from := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	want := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	if got := s.Next(from); !got.Equal(want) {
		t.Fatalf("Cron DOM: got %v want %v", got, want)
	}
}

func TestCronTimezone(t *testing.T) {
	t.Parallel()
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("tz database unavailable: %v", err)
	}
	// "0 9 * * *" = daily 09:00 local. In winter (CET = UTC+1) that
	// is 08:00 UTC; the returned instant is normalised to UTC.
	s, err := Cron("0 9 * * *", berlin)
	if err != nil {
		t.Fatalf("Cron: %v", err)
	}
	from := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	want := time.Date(2024, 1, 15, 8, 0, 0, 0, time.UTC)
	got := s.Next(from)
	if !got.Equal(want) {
		t.Fatalf("Cron TZ winter: got %v want %v", got, want)
	}
	if got.Location() != time.UTC {
		t.Fatalf("Cron Next must return UTC, got location %v", got.Location())
	}
}

func TestCronDSTSpringForward(t *testing.T) {
	t.Parallel()
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("tz database unavailable: %v", err)
	}
	// EU DST 2024: spring-forward on Sun 2024-03-31 (02:00 CET →
	// 03:00 CEST). A daily "0 9 * * *" fires at 09:00 local on both
	// sides — the UTC offset shifts from +1 to +2 across the boundary.
	s, err := Cron("0 9 * * *", berlin)
	if err != nil {
		t.Fatalf("Cron: %v", err)
	}
	// March 30 (CET, UTC+1): 09:00 local = 08:00 UTC.
	beforeFrom := time.Date(2024, 3, 30, 0, 0, 0, 0, time.UTC)
	beforeWant := time.Date(2024, 3, 30, 8, 0, 0, 0, time.UTC)
	if got := s.Next(beforeFrom); !got.Equal(beforeWant) {
		t.Fatalf("Cron DST before: got %v want %v", got, beforeWant)
	}
	// March 31 (CEST, UTC+2): 09:00 local = 07:00 UTC.
	afterFrom := time.Date(2024, 3, 30, 8, 0, 0, 1, time.UTC)
	afterWant := time.Date(2024, 3, 31, 7, 0, 0, 0, time.UTC)
	if got := s.Next(afterFrom); !got.Equal(afterWant) {
		t.Fatalf("Cron DST after: got %v want %v", got, afterWant)
	}
}

func TestCronNilLocationDefaultsUTC(t *testing.T) {
	t.Parallel()
	s, err := Cron("0 9 * * *", nil)
	if err != nil {
		t.Fatalf("Cron: %v", err)
	}
	from := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	want := time.Date(2024, 1, 15, 9, 0, 0, 0, time.UTC)
	if got := s.Next(from); !got.Equal(want) {
		t.Fatalf("Cron nil loc (UTC): got %v want %v", got, want)
	}
}
