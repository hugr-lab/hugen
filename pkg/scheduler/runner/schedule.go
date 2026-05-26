package runner

import "time"

// Every fires the fn at fixed intervals. Next(after) returns
// after.Add(d) — i.e. the cadence is anchored to the previous
// tick's `now`, not to a wall-clock baseline. That keeps
// reapers running roughly every d regardless of clock drift or
// Pause durations.
//
// d must be > 0; zero or negative durations cause Next to return
// the zero time (interpreted by the Runner as "no further fires").
func Every(d time.Duration) Schedule {
	return everySchedule{d: d}
}

type everySchedule struct {
	d time.Duration
}

func (s everySchedule) Next(after time.Time) time.Time {
	if s.d <= 0 {
		return time.Time{}
	}
	return after.Add(s.d)
}

// Once fires at exactly at and never again. Once at has passed
// the schedule reports zero — the Runner keeps the registration
// installed (consumers can re-Schedule via Unregister + Register)
// but never dispatches it. Useful for one-shot reminder timers in
// Phase 6.1b (the "wake" task kind).
func Once(at time.Time) Schedule {
	return onceSchedule{at: at}
}

type onceSchedule struct {
	at time.Time
}

func (s onceSchedule) Next(after time.Time) time.Time {
	if s.at.After(after) {
		return s.at
	}
	return time.Time{}
}
