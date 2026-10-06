package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestClassifyWorkerCommand(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want workerCommandDisposition
	}{
		{name: "auth status", args: []string{"auth", "status"}, want: workerCommandForward},
		{name: "photos authorization", args: []string{"--timeout", "2s", "auth", "photos", "--request"}, want: workerCommandForward},
		{name: "album removal", args: []string{"albums", "remove", "--album-id", "a", "--photo-id", "p", "--execute"}, want: workerCommandForward},
		{name: "duplicates", args: []string{"albums", "duplicates", "--album-id", "a"}, want: workerCommandForward},
		{name: "hide", args: []string{"photos", "hide", "--photo-id", "p", "--reason", "blur"}, want: workerCommandForward},
		{name: "set-date", args: []string{"photos", "set-date", "--dates-file", "d.json"}, want: workerCommandForward},
		{name: "hidden list", args: []string{"photos", "hidden", "list"}, want: workerCommandForward},
		{name: "help stays local", args: []string{"--help"}, want: workerCommandLocal},
		{name: "version stays local", args: []string{"--json", "version"}, want: workerCommandLocal},
		{name: "worker nesting rejected", args: []string{"worker"}, want: workerCommandReject},
		{name: "shell rejected", args: []string{"sh", "-c", "id"}, want: workerCommandReject},
		{name: "unknown album command rejected", args: []string{"albums", "shell"}, want: workerCommandReject},
		{name: "malformed global rejected", args: []string{"--timeout"}, want: workerCommandReject},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := classifyWorkerCommand(tt.args)
			if got != tt.want {
				t.Fatalf("disposition=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestWorkerMutationDetectionUsesEffectiveExecuteFlag(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{args: []string{"albums", "remove", "--execute"}, want: true},
		{args: []string{"albums", "remove", "--execute=TRUE"}, want: true},
		{args: []string{"albums", "remove", "-execute=1"}, want: true},
		{args: []string{"albums", "remove", "--execute=false"}, want: false},
		{args: []string{"albums", "import", "--skip-duplicate-check", "--execute"}, want: true},
		{args: []string{"albums", "import", "--skip-duplicate-check", "--execute=false"}, want: false},
		{args: []string{"albums", "remove", "--execute", "--execute=false"}, want: false},
		{args: []string{"albums", "list", "--execute"}, want: false},
		{args: []string{"photos", "hide", "--execute"}, want: true},
		{args: []string{"photos", "set-date", "--execute"}, want: true},
		{args: []string{"photos", "set-date", "--dry-run"}, want: false},
		{args: []string{"photos", "unhide", "--execute=false"}, want: false},
		{args: []string{"auth", "photos", "--request"}, want: false},
		{args: []string{"photos", "hide", "--photo-id", "ID", "--execute", "--reason", "--execute=false"}, want: true},
		{args: []string{"albums", "remove", "--album-id", "--execute", "--photo-id", "P"}, want: false},
	}
	for _, tt := range tests {
		if got := workerCommandMutates(tt.args); got != tt.want {
			t.Errorf("workerCommandMutates(%q)=%v, want %v", tt.args, got, tt.want)
		}
	}
}

func TestNormalizeWorkerPathArgsUsesCallerWorkingDirectory(t *testing.T) {
	workingDirectory := "/Users/example/project"
	args := []string{
		"albums", "import",
		"--file", "photos/one.jpg",
		"-file", "photos/single-dash.jpg",
		"--file=/shared/two.jpg",
		"--files-file=manifests/import.json",
		"--photo-ids-file", "ids.txt",
		"--images-file=scan/images.json",
		"--output-dir", "out",
		"--dates-file", "dates.json",
	}
	got, err := normalizeWorkerPathArgsFrom(args, workingDirectory)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"albums", "import",
		"--file", "/Users/example/project/photos/one.jpg",
		"-file", "/Users/example/project/photos/single-dash.jpg",
		"--file=/shared/two.jpg",
		"--files-file=/Users/example/project/manifests/import.json",
		"--photo-ids-file", "/Users/example/project/ids.txt",
		"--images-file=/Users/example/project/scan/images.json",
		"--output-dir", "/Users/example/project/out",
		"--dates-file", "/Users/example/project/dates.json",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized args=%q, want %q", got, want)
	}
	if reflect.DeepEqual(args, got) {
		t.Fatal("normalization should return a copy")
	}
	if err := validateWorkerPathsAbsolute(got); err != nil {
		t.Fatalf("normalized paths were not accepted by worker: %v", err)
	}
	if err := validateWorkerPathsAbsolute(args); err == nil {
		t.Fatal("worker should reject a client-relative path")
	}
	opaque, err := normalizeWorkerPathArgsFrom(
		[]string{"albums", "create", "--name", "--file"}, workingDirectory)
	if err != nil || !reflect.DeepEqual(opaque, []string{"albums", "create", "--name", "--file"}) {
		t.Fatalf("non-path flag value was reinterpreted: args=%q err=%v", opaque, err)
	}
}

func TestWorkerRequestFramingIsStrictAndBounded(t *testing.T) {
	valid, err := decodeWorkerRequest([]byte(`{"version":1,"deadlineUnixMillis":4102444800000,"args":["albums","list"]}`))
	if err != nil || !reflect.DeepEqual(valid.Args, []string{"albums", "list"}) {
		t.Fatalf("valid request=%#v err=%v", valid, err)
	}
	for name, payload := range map[string]string{
		"unknown field":    `{"version":1,"deadlineUnixMillis":4102444800000,"args":["albums","list"],"command":"shell"}`,
		"multiple values":  `{"version":1,"deadlineUnixMillis":4102444800000,"args":["albums","list"]}{}`,
		"wrong version":    `{"version":2,"deadlineUnixMillis":4102444800000,"args":["albums","list"]}`,
		"missing deadline": `{"version":1,"args":["albums","list"]}`,
		"null args":        `{"version":1,"deadlineUnixMillis":4102444800000,"args":null}`,
		"nul argument":     `{"version":1,"deadlineUnixMillis":4102444800000,"args":["albums","li\u0000st"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeWorkerRequest([]byte(payload)); err == nil {
				t.Fatal("expected request to be rejected")
			}
		})
	}
	if _, err := readBounded(strings.NewReader(strings.Repeat("x", maxWorkerRequestBytes+1)), maxWorkerRequestBytes); err == nil {
		t.Fatal("expected oversized frame to be rejected")
	}
}

func TestPrepareWorkerSocketProtectsAndOwnsPath(t *testing.T) {
	paths := testWorkerPaths(t)
	listener, owned, err := prepareWorkerSocket(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = listener.Close()
		cleanupOwnedSocket(paths.socket, owned)
	}()

	directoryInfo, err := os.Lstat(paths.directory)
	if err != nil {
		t.Fatal(err)
	}
	socketInfo, err := os.Lstat(paths.socket)
	if err != nil {
		t.Fatal(err)
	}
	if got := directoryInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("directory mode=%#o, want 0700", got)
	}
	if got := socketInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("socket mode=%#o, want 0600", got)
	}
	if _, _, err := prepareWorkerSocket(paths); err == nil || !strings.Contains(err.Error(), "already owns") {
		t.Fatalf("expected active owner rejection, got %v", err)
	}
}

func TestPrepareWorkerSocketReplacesOnlyOwnedStaleSocket(t *testing.T) {
	t.Run("owned stale socket", func(t *testing.T) {
		paths := testWorkerPaths(t)
		first, _, err := prepareWorkerSocket(paths)
		if err != nil {
			t.Fatal(err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		second, owned, err := prepareWorkerSocket(paths)
		if err != nil {
			t.Fatalf("replace stale socket: %v", err)
		}
		_ = second.Close()
		cleanupOwnedSocket(paths.socket, owned)
	})

	t.Run("ordinary file", func(t *testing.T) {
		paths := testWorkerPaths(t)
		if err := os.MkdirAll(paths.directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paths.socket, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := prepareWorkerSocket(paths); err == nil {
			t.Fatal("expected ordinary socket-path file to be rejected")
		}
		data, err := os.ReadFile(paths.socket)
		if err != nil || string(data) != "keep" {
			t.Fatalf("ordinary file changed: data=%q err=%v", data, err)
		}
	})
}

func TestForwardingConfigurationIsFailClosed(t *testing.T) {
	paths := testWorkerPaths(t)
	var stdout, stderr bytes.Buffer
	code, handled := forwardToWorkerAt(paths, []string{"albums", "list"}, &stdout, &stderr)
	if code != 0 || handled {
		t.Fatalf("unconfigured worker: code=%d handled=%v stderr=%q", code, handled, stderr.String())
	}
	writeWorkerMarker(t, paths)
	code, handled = forwardToWorkerAt(paths, []string{"albums", "list"}, &stdout, &stderr)
	if code == 0 || !handled || !strings.Contains(stderr.String(), "worker_unavailable") {
		t.Fatalf("configured worker fallback was not closed: code=%d handled=%v stderr=%q", code, handled, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code, handled = forwardToWorkerAt(paths, []string{"version"}, &stdout, &stderr)
	if code != 0 || handled {
		t.Fatalf("version should stay local: code=%d handled=%v stderr=%q", code, handled, stderr.String())
	}
}

func TestUnconfiguredWorkerLeavesUsageErrorsLocal(t *testing.T) {
	for _, args := range [][]string{{"albums"}, {"albums", "unknown"}} {
		paths := testWorkerPaths(t)
		var stdout, stderr bytes.Buffer
		code, handled := forwardToWorkerAt(paths, args, &stdout, &stderr)
		if code != 0 || handled || stderr.Len() != 0 {
			t.Fatalf("unconfigured args=%q code=%d handled=%v stderr=%q", args, code, handled, stderr.String())
		}
		if code := run(args, &stdout, &stderr, nil); code != 2 {
			t.Fatalf("local usage args=%q code=%d stderr=%q", args, code, stderr.String())
		}
		stderr.Reset()
		writeWorkerMarker(t, paths)
		code, handled = forwardToWorkerAt(paths, args, &stdout, &stderr)
		if code == 0 || !handled || !strings.Contains(stderr.String(), "worker_command_rejected") {
			t.Fatalf("configured args=%q code=%d handled=%v stderr=%q", args, code, handled, stderr.String())
		}
	}
}

func TestWorkerMarkerAndSocketMustStayPrivate(t *testing.T) {
	t.Run("marker", func(t *testing.T) {
		paths := testWorkerPaths(t)
		writeWorkerMarker(t, paths)
		if err := os.Chmod(paths.marker, 0o644); err != nil {
			t.Fatal(err)
		}
		configured, err := workerIsConfigured(paths)
		if !configured || err == nil || !strings.Contains(err.Error(), "other users") {
			t.Fatalf("configured=%v err=%v", configured, err)
		}
	})

	t.Run("socket", func(t *testing.T) {
		paths := testWorkerPaths(t)
		listener, owned, err := prepareWorkerSocket(paths)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = listener.Close()
			cleanupOwnedSocket(paths.socket, owned)
		}()
		if err := os.Chmod(paths.socket, 0o666); err != nil {
			t.Fatal(err)
		}
		if err := validateWorkerSocket(paths); err == nil || !strings.Contains(err.Error(), "other users") {
			t.Fatalf("expected unsafe socket mode rejection, got %v", err)
		}
	})
}

func TestWorkerRoundTripUsesInjectedRunner(t *testing.T) {
	paths := testWorkerPaths(t)
	writeWorkerMarker(t, paths)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	received := make(chan []string, 1)
	go func() {
		done <- serveWorker(ctx, paths, func(args []string, stdout, stderr io.Writer) int {
			received <- append([]string(nil), args...)
			_, _ = io.WriteString(stdout, "remote stdout\n")
			_, _ = io.WriteString(stderr, "remote stderr\n")
			return 7
		})
	}()
	waitForWorkerSocket(t, paths.socket)

	var stdout, stderr bytes.Buffer
	args := []string{"albums", "remove", "--album-id", "album", "--photo-ids-file", "review/ids.json"}
	code, handled := forwardToWorkerAt(paths, args, &stdout, &stderr)
	if code != 7 || !handled || stdout.String() != "remote stdout\n" || stderr.String() != "remote stderr\n" {
		t.Fatalf("code=%d handled=%v stdout=%q stderr=%q", code, handled, stdout.String(), stderr.String())
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := append([]string(nil), args...)
	wantPrefix[len(wantPrefix)-1] = filepath.Join(workingDirectory, "review/ids.json")
	got := <-received
	if len(got) != len(wantPrefix)+1 || !reflect.DeepEqual(got[:len(wantPrefix)], wantPrefix) ||
		!strings.HasPrefix(got[len(got)-1], "--timeout=") {
		t.Fatalf("runner args=%q, want prefix %q plus capped timeout", got, wantPrefix)
	}
	_, cappedTimeout, err := parseGlobals(got)
	if err != nil || cappedTimeout <= 0 || cappedTimeout >= 60*time.Second {
		t.Fatalf("runner timeout=%s err=%v; expected remaining request budget", cappedTimeout, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop")
	}
}

func TestSecondWorkerRefusesWhileFirstHoldsLock(t *testing.T) {
	paths := testWorkerPaths(t)
	writeWorkerMarker(t, paths)
	lock, err := acquireWorkerLock(paths.directory)
	if err != nil {
		t.Fatal(err)
	}
	// A stopping worker has already removed its socket but still holds the
	// lock while it finishes a request.
	err = serveWorker(context.Background(), paths, func([]string, io.Writer, io.Writer) int { return 0 })
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("second worker err=%v, want refusal while the lock is held", err)
	}
	if _, statErr := os.Lstat(paths.socket); !os.IsNotExist(statErr) {
		t.Fatalf("second worker created a socket: %v", statErr)
	}
	_ = lock.Close()
	relock, err := acquireWorkerLock(paths.directory)
	if err != nil {
		t.Fatalf("lock not released with its holder: %v", err)
	}
	_ = relock.Close()
}

func TestExpiredQueuedRequestNeverCallsRunner(t *testing.T) {
	paths := testWorkerPaths(t)
	if err := os.MkdirAll(paths.directory, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: paths.socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	defer func() {
		_ = listener.Close()
		_ = os.Remove(paths.socket)
	}()
	runnerCalls := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			return
		}
		handleWorkerConnection(connection, func([]string, io.Writer, io.Writer) int {
			runnerCalls++
			return 0
		})
	}()

	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: paths.socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(workerRequest{
		Version:            workerProtocolVersion,
		DeadlineUnixMillis: time.Now().Add(-time.Second).UnixMilli(),
		Args:               []string{"albums", "list"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	responsePayload, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	<-done
	response, err := decodeWorkerResponse(responsePayload)
	if err != nil {
		t.Fatalf("decode response: %v; payload=%q", err, responsePayload)
	}
	if runnerCalls != 0 || response.Error == nil || response.Error.Code != "worker_request_expired" {
		t.Fatalf("runnerCalls=%d response=%#v", runnerCalls, response)
	}
}

func TestLostMutationResponseRequiresReadbackWithoutRetry(t *testing.T) {
	paths := testWorkerPaths(t)
	writeWorkerMarker(t, paths)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: paths.socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(paths.socket, 0o600); err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{}, 1)
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr == nil {
			_, _ = io.ReadAll(connection)
			accepted <- struct{}{}
			_ = connection.Close() // Deliberately lose the response after submission.
		}
	}()

	var stdout, stderr bytes.Buffer
	code, handled := forwardToWorkerAt(paths,
		[]string{"albums", "remove", "--album-id", "a", "--photo-id", "p", "--execute"},
		&stdout, &stderr)
	if code == 0 || !handled {
		t.Fatalf("code=%d handled=%v", code, handled)
	}
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("request was not submitted")
	}
	var envelope struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error response: %v; output=%q", err, stderr.String())
	}
	if envelope.Error.Code != "worker_outcome_unknown" || envelope.Error.Details["readbackRequired"] != true {
		t.Fatalf("unexpected lost-response error: %#v", envelope.Error)
	}
	_ = listener.Close()
	_ = os.Remove(paths.socket)
}

func TestRunnerPanicIsContainedAndMarksMutationUnknown(t *testing.T) {
	response := executeWorkerRequest(
		[]string{"albums", "remove", "--album-id", "a", "--photo-id", "p", "--execute"},
		func([]string, io.Writer, io.Writer) int { panic("boom") },
	)
	if response.Error == nil || response.Error.Code != "worker_runner_panic" || !response.Error.ReadbackRequired {
		t.Fatalf("unexpected panic response: %#v", response)
	}
}

func testWorkerPaths(t *testing.T) workerPaths {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "pcw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	directory := filepath.Join(base, "photoscli")
	return workerPaths{
		directory: directory,
		marker:    filepath.Join(directory, workerMarkerFilename),
		socket:    filepath.Join(directory, workerSocketFilename),
	}
}

func writeWorkerMarker(t *testing.T, paths workerPaths) {
	t.Helper()
	if err := os.MkdirAll(paths.directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(paths.directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.marker, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitForWorkerSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		info, err := os.Lstat(path)
		if err == nil && info.Mode()&os.ModeSocket != 0 {
			return
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("worker socket was not created")
}
