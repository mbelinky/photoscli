package app

import (
	"context"
	"reflect"
	"testing"
)

func TestImportFilesPassesSkipDuplicateCheckFirst(t *testing.T) {
	original := appleScriptRunner
	t.Cleanup(func() { appleScriptRunner = original })
	var got []string
	appleScriptRunner = func(_ context.Context, operation, script string, arguments []string) (string, error) {
		if operation != "albums.import" || script != importScript {
			t.Fatalf("unexpected script invocation: operation=%q", operation)
		}
		got = append([]string(nil), arguments...)
		return "", nil
	}

	_, err := (OsaBackend{}).ImportFiles(context.Background(), "album-1", []string{"/tmp/one.jpg", "/tmp/two.jpg"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"true", "album-1", "/tmp/one.jpg", "/tmp/two.jpg"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("arguments=%q, want %q", got, want)
	}
}
