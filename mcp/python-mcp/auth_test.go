package main

import (
	"context"
	"testing"
)

func TestLoadAuthSource_AllSet(t *testing.T) {
	t.Setenv("HUGR_URL", "http://hugr")
	t.Setenv("HUGR_ACCESS_TOKEN", "boot")
	t.Setenv("HUGR_TOKEN_URL", "http://loop/api/auth/agent-token")
	a, err := loadAuthSource(discardLogger())
	if err != nil {
		t.Fatalf("loadAuthSource: %v", err)
	}
	if a == nil || a.store == nil {
		t.Fatalf("expected configured authSource, got nil")
	}
	if a.hugrURL != "http://hugr" {
		t.Errorf("hugrURL = %q", a.hugrURL)
	}
}

func TestLoadAuthSource_AllUnset(t *testing.T) {
	t.Setenv("HUGR_URL", "")
	t.Setenv("HUGR_ACCESS_TOKEN", "")
	t.Setenv("HUGR_TOKEN_URL", "")
	a, err := loadAuthSource(discardLogger())
	if err != nil {
		t.Fatalf("loadAuthSource: %v", err)
	}
	if a != nil {
		t.Errorf("no-Hugr path should yield nil authSource, got %+v", a)
	}
}

func TestLoadAuthSource_PartialIsTreatedAsUnset(t *testing.T) {
	t.Setenv("HUGR_URL", "http://hugr")
	t.Setenv("HUGR_ACCESS_TOKEN", "")
	t.Setenv("HUGR_TOKEN_URL", "")
	a, err := loadAuthSource(discardLogger())
	if err != nil {
		t.Fatalf("loadAuthSource: %v", err)
	}
	if a != nil {
		t.Errorf("partial env should be treated as unset, got %+v", a)
	}
}

func TestCurrentToken_NilAuth(t *testing.T) {
	var a *authSource
	url, tok, err := a.currentToken(context.Background())
	if err != nil || url != "" || tok != "" {
		t.Errorf("nil authSource should yield empty triple, got %q %q %v", url, tok, err)
	}
}
