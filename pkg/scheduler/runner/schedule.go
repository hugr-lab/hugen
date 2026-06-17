package runner

import (
	"fmt"
	"time"

	robfig "github.com/robfig/cron/v3"
)

// cronParser parses standard 5-field cron expressions (minute hour
// dom month dow) — no seconds field, matching crontab(5). Shared
// across all cronSchedule instances; robfig parsers are stateless.
var cronParser = robfig.NewParser(robfig.Minute | robfig.Hour | robfig.Dom | robfig.Month | robfig.Dow)

// Cron fires on a 5-field standard cron expression evaluated in loc
// (UTC when loc is nil). Wraps robfig/cron/v3. The expression is
// validated here, so an invalid spec is a constructor error — callers
// (schedule:create) surface it as a tool error rather than letting a
// broken schedule reach the Runner. `"0 9 * * 1"` = every Monday at
// 09:00 in loc.
func Cron(expr string, loc *time.Location) (Schedule, error) {
	inner, err := cronParser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression %q: %w", expr, err)
	}
	if loc == nil {
		loc = time.UTC
	}
	return cronSchedule{inner: inner, loc: loc}, nil
}

type cronSchedule struct {
	inner robfig.Schedule
	loc   *time.Location
}

// Next returns the next fire instant strictly after `after`,
// evaluated in the schedule's location. robfig's Next is exclusive
// on the boundary (it never returns `after` itself), which matches
// our recurring semantics — each fire advances past the planned
// instant. The result is normalised to UTC so callers persist a
// consistent wall-clock-independent timestamp.
func (s cronSchedule) Next(after time.Time) time.Time {
	return s.inner.Next(after.In(s.loc)).UTC()
}

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

// Once fires at exactly at and never again. The semantics are
// inclusive on the boundary: a registration created with
// `Once(now)` fires on the next tick rather than becoming a
// dead row with nextFireAt=zero. Once `at` has passed (i.e. the
// schedule was already consulted at or after `at`) Next returns
// zero — the Runner keeps the registration installed but skips
// it forever. Useful for one-shot reminder timers in Phase 6.1b
// (the "wake" task kind).
func Once(at time.Time) Schedule {
	return onceSchedule{at: at}
}

type onceSchedule struct {
	at time.Time
}

func (s onceSchedule) Next(after time.Time) time.Time {
	if !s.at.Before(after) {
		return s.at
	}
	return time.Time{}
}

// Manual is an inert schedule: Next always returns the zero time, so
// the registration never advances on its own. The next fire instant is
// set explicitly — by [WithInitialFireAt] at registration and by
// [Runner.Reschedule] thereafter. Used by schedule-driven extensions
// (the scheduler ext) that own their cadence from a durable plan and
// must fire overdue instants verbatim, which the past-dropping [Once]
// cannot do.
func Manual() Schedule {
	return manualSchedule{}
}

type manualSchedule struct{}

func (manualSchedule) Next(time.Time) time.Time { return time.Time{} }
