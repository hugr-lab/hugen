package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func refreshRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/skills/refresh", nil)
}

func TestHandleRefreshSkills_NotConfigured(t *testing.T) {
	a := &Adapter{logger: slog.Default()} // refreshSkills nil ⇒ 501
	rec := httptest.NewRecorder()
	a.handleRefreshSkills(rec, refreshRequest())
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501 (no marketplace)", rec.Code)
	}
}

func TestHandleRefreshSkills_OK(t *testing.T) {
	called := false
	a := &Adapter{
		logger: slog.Default(),
		refreshSkills: func(context.Context) (any, error) {
			called = true
			return map[string]int{"downloaded": 2, "removed": 1, "failed": 0}, nil
		},
	}
	rec := httptest.NewRecorder()
	a.handleRefreshSkills(rec, refreshRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if !called {
		t.Fatal("refresher not invoked")
	}
	var out map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["downloaded"] != 2 || out["removed"] != 1 {
		t.Errorf("body = %v, want downloaded=2 removed=1", out)
	}
}

func TestHandleRefreshSkills_Error(t *testing.T) {
	a := &Adapter{
		logger:        slog.Default(),
		refreshSkills: func(context.Context) (any, error) { return nil, errors.New("hub unreachable") },
	}
	rec := httptest.NewRecorder()
	a.handleRefreshSkills(rec, refreshRequest())
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", rec.Code)
	}
}
