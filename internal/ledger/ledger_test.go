package ledger

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestReadHiddenReplaysFiltersAndSkipsCorruptLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	entries := []Entry{
		{At: now.Add(-3 * time.Hour), Action: "hide", PhotoID: "already", Reason: "blur", Changed: false, Verified: true},
		{At: now.Add(-2 * time.Hour), Action: "hide", PhotoID: "one", Reason: "blur", Batch: "a", Changed: true, Verified: true},
		{At: now.Add(-time.Hour), Action: "hide", PhotoID: "two", Reason: "blur", Verified: false},
		{At: now.Add(-time.Hour), Action: "unhide", PhotoID: "one", Reason: "review", Verified: true},
		{At: now.Add(-30 * time.Minute), Action: "hide", PhotoID: "three", Reason: "screens", Batch: "b", Changed: true, Verified: true},
	}
	if err := Append(path, entries); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("bad json\n")
	_ = f.Close()
	got, err := ReadHidden(path, "screens", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.CorruptLines != 1 || !reflect.DeepEqual([]string{got.Photos[0].PhotoID}, []string{"three"}) {
		t.Fatalf("got=%+v", got)
	}
	all, err := ReadHidden(path, "", 0, now)
	if err != nil || len(all.Photos) != 1 || all.Photos[0].PhotoID != "three" {
		t.Fatalf("all=%+v err=%v", all, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v", info.Mode(), err)
	}
}

func TestParseSinceAcceptsDaysAndGoDurations(t *testing.T) {
	for value, want := range map[string]time.Duration{"": 0, "30d": 30 * 24 * time.Hour, "72h": 72 * time.Hour} {
		got, err := ParseSince(value)
		if err != nil || got != want {
			t.Fatalf("ParseSince(%q) = %v, %v; want %v", value, got, err, want)
		}
	}
	for _, value := range []string{"-1d", "xd", "-2h", "soon"} {
		if _, err := ParseSince(value); err == nil {
			t.Fatalf("ParseSince(%q) accepted an invalid value", value)
		}
	}
}
