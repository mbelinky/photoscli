package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseGlobalsPreservesCommandFlagValues(t *testing.T) {
	for _, nameFlag := range []string{"--name", "-name"} {
		args, timeout, err := parseGlobals([]string{"albums", "create", nameFlag, "--json", "--timeout", "9s", "--json"})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"albums", "create", nameFlag, "--json"}
		if !reflect.DeepEqual(args, want) || timeout != 9*time.Second {
			t.Fatalf("got args=%q timeout=%s", args, timeout)
		}
	}
}

func TestSkipDuplicateCheckIsABooleanCommandFlag(t *testing.T) {
	if commandFlagTakesValue("--skip-duplicate-check") {
		t.Fatal("--skip-duplicate-check must not consume the following argument")
	}
}

func TestReadFilesRequiresJSONArray(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.json")
	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(valid, []byte(`["one.jpg","two.jpg"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalid, []byte("one.jpg\ntwo.jpg\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := readFiles(valid)
	if err != nil || !reflect.DeepEqual(paths, []string{"one.jpg", "two.jpg"}) {
		t.Fatalf("paths=%q err=%v", paths, err)
	}
	if _, err := readFiles(invalid); err == nil {
		t.Fatal("expected non-JSON file list to fail")
	}
}

func TestCommandHelpReturnsSuccessWithoutBackendAccess(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"albums", "create", "--help"}, &stdout, &stderr, nil)
	if code != 0 || !strings.Contains(stdout.String(), "Usage: photoscli albums create") || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestAlbumsUsageListsEveryAlbumCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"albums"}, &stdout, &stderr, nil)
	if code != 2 || !strings.Contains(stderr.String(), "list, get, create, add, import, duplicates, remove") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}
