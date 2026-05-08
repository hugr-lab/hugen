package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadDotEnv_MissingFile(t *testing.T) {
	kv, err := readDotEnv(filepath.Join(t.TempDir(), "absent.env"))
	if err != nil {
		t.Fatalf("readDotEnv: %v", err)
	}
	if len(kv) != 0 {
		t.Errorf("missing file should yield empty map, got %v", kv)
	}
}

func TestReadDotEnv_StripsQuotes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	body := "# comment\n" +
		"FOO=plain\n" +
		"BAR=\"quoted value\"\n" +
		"BAZ=  trimmed  \n" +
		"\n" +
		"NO_EQ_LINE\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	kv, err := readDotEnv(path)
	if err != nil {
		t.Fatalf("readDotEnv: %v", err)
	}
	if kv["FOO"] != "plain" {
		t.Errorf("FOO = %q", kv["FOO"])
	}
	if kv["BAR"] != "quoted value" {
		t.Errorf("BAR = %q", kv["BAR"])
	}
	if kv["BAZ"] != "trimmed" {
		t.Errorf("BAZ = %q", kv["BAZ"])
	}
	if _, ok := kv["NO_EQ_LINE"]; ok {
		t.Errorf("malformed line leaked into map")
	}
}

func TestWriteDotEnv_PreservesCommentsAndUnrelatedKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	original := "# top comment\n" +
		"\n" +
		"OTHER=keep_me\n" +
		"HUGR_ACCESS_TOKEN=stale\n" +
		"# inline comment\n" +
		"FOO=bar\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	updates := map[string]string{
		"HUGR_ACCESS_TOKEN":     "fresh",
		"HUGR_REFRESH_TOKEN":    "rfh",
		"HUGR_TOKEN_EXPIRES_AT": "2026-01-01T00:00:00Z",
	}
	if err := writeDotEnv(path, updates); err != nil {
		t.Fatalf("writeDotEnv: %v", err)
	}

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	for _, want := range []string{
		"# top comment",
		"OTHER=keep_me",
		"# inline comment",
		"FOO=bar",
		"HUGR_ACCESS_TOKEN=fresh",
		"HUGR_REFRESH_TOKEN=rfh",
		"HUGR_TOKEN_EXPIRES_AT=2026-01-01T00:00:00Z",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing line %q in output:\n%s", want, got)
		}
	}
	if strings.Contains(got, "HUGR_ACCESS_TOKEN=stale") {
		t.Errorf("stale line not replaced:\n%s", got)
	}
}

func TestWriteDotEnv_CreatesNewFileWithOrderedKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fresh", ".env")

	if err := writeDotEnv(path, map[string]string{
		"HUGR_REFRESH_TOKEN": "r",
		"HUGR_ACCESS_TOKEN":  "a",
		"HUGR_URL":           "http://x",
	}); err != nil {
		t.Fatalf("writeDotEnv: %v", err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimRight(string(out), "\n")
	want := "HUGR_ACCESS_TOKEN=a\nHUGR_REFRESH_TOKEN=r\nHUGR_URL=http://x"
	if got != want {
		t.Errorf("ordered output mismatch:\n got: %q\nwant: %q", got, want)
	}
}
