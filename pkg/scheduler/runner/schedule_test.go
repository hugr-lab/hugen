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
	if got := s.Next(target); !got.IsZero() {
		t.Fatalf("Once.Next at target should be zero, got %v", got)
	}
	if got := s.Next(target.Add(time.Second)); !got.IsZero() {
		t.Fatalf("Once.Next after target should be zero, got %v", got)
	}
}
