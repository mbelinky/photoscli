package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mbelinky/photoscli/internal/app"
)

const (
	workerServiceDirectoryName = "photoscli"
	workerMarkerFilename       = "worker.enabled"
	workerSocketFilename       = "worker.sock"
	workerLockFilename         = "worker.lock"

	workerProtocolVersion  = 1
	maxWorkerRequestBytes  = 64 << 10
	maxWorkerResponseBytes = 128 << 20
	maxWorkerOutputBytes   = 32 << 20
	maxWorkerArgumentCount = 4096
)

var (
	workerIdleTimeout = 10 * time.Second
	workerDialTimeout = 2 * time.Second
	workerResultGrace = 5 * time.Second
)

type workerPaths struct {
	directory string
	marker    string
	socket    string
}

type workerRequest struct {
	Version            int      `json:"version"`
	DeadlineUnixMillis int64    `json:"deadlineUnixMillis"`
	Args               []string `json:"args"`
}

type workerResponse struct {
	Version  int                  `json:"version"`
	ExitCode int                  `json:"exitCode"`
	Stdout   string               `json:"stdout,omitempty"`
	Stderr   string               `json:"stderr,omitempty"`
	Error    *workerProtocolError `json:"error,omitempty"`
}

type workerProtocolError struct {
	Code             string `json:"code"`
	Message          string `json:"message"`
	ReadbackRequired bool   `json:"readbackRequired,omitempty"`
}

type workerRunner func(args []string, stdout, stderr io.Writer) int

type workerCommandDisposition int

const (
	workerCommandLocal workerCommandDisposition = iota
	workerCommandForward
	workerCommandReject
)

// workerServicePaths is the single path contract shared with the installer.
// On macOS it resolves to ~/Library/Application Support/photoscli.
func workerServicePaths() (directory, marker, socket string, err error) {
	configDirectory, err := os.UserConfigDir()
	if err != nil {
		return "", "", "", fmt.Errorf("resolve user application support directory: %w", err)
	}
	directory = filepath.Join(configDirectory, workerServiceDirectoryName)
	return directory,
		filepath.Join(directory, workerMarkerFilename),
		filepath.Join(directory, workerSocketFilename), nil
}

func defaultWorkerPaths() (workerPaths, error) {
	directory, marker, socket, err := workerServicePaths()
	return workerPaths{directory: directory, marker: marker, socket: socket}, err
}

// runWorker starts the foreground worker. The LaunchAgent owns its lifetime.
// The hidden worker command accepts no additional arguments.
func runWorker(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		return writeError(stderr, "worker", usageError("worker accepts no arguments"))
	}
	paths, err := defaultWorkerPaths()
	if err != nil {
		return writeWorkerError(stderr, "worker_path_error", err.Error(), false)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = serveWorker(ctx, paths, func(args []string, commandStdout, commandStderr io.Writer) int {
		return run(args, commandStdout, commandStderr, app.OsaBackend{})
	})
	if err != nil {
		return writeWorkerError(stderr, "worker_start_failed", err.Error(), false)
	}
	return 0
}

// forwardToWorker forwards Photos commands only after the installer creates the
// private marker. Safe local commands return handled=false. Once configured,
// connection failures are returned instead of falling back to this process.
func forwardToWorker(args []string, stdout, stderr io.Writer) (code int, handled bool) {
	paths, err := defaultWorkerPaths()
	if err != nil {
		return writeWorkerError(stderr, "worker_path_error", err.Error(), false), true
	}
	return forwardToWorkerAt(paths, args, stdout, stderr)
}

func forwardToWorkerAt(paths workerPaths, args []string, stdout, stderr io.Writer) (code int, handled bool) {
	disposition, commandErr := classifyWorkerCommand(args)
	if disposition == workerCommandLocal {
		return 0, false
	}

	configured, err := workerIsConfigured(paths)
	if err != nil {
		return writeWorkerError(stderr, "worker_configuration_invalid", err.Error(), false), true
	}
	if !configured {
		return 0, false
	}
	if disposition == workerCommandReject {
		if commandErr == nil {
			commandErr = errors.New("command is not allowed through the Photos worker")
		}
		return writeWorkerError(stderr, "worker_command_rejected", commandErr.Error(), false), true
	}
	if err := validateWorkerSocket(paths); err != nil {
		return writeWorkerError(stderr, "worker_unavailable", err.Error(), false), true
	}

	forwardedArgs, err := normalizeWorkerPathArgs(args)
	if err != nil {
		return writeWorkerError(stderr, "worker_bad_request", err.Error(), false), true
	}
	response, submitted, err := callWorker(paths.socket, forwardedArgs)
	if err != nil {
		if submitted && workerCommandMutates(args) {
			return writeWorkerError(stderr, "worker_outcome_unknown",
				"the worker connection was lost after submitting a mutation; inspect the album before retrying",
				true), true
		}
		return writeWorkerError(stderr, "worker_unavailable", err.Error(), false), true
	}
	if response.Error != nil {
		return writeWorkerError(stderr, response.Error.Code, response.Error.Message,
			response.Error.ReadbackRequired), true
	}
	_, _ = io.WriteString(stdout, response.Stdout)
	_, _ = io.WriteString(stderr, response.Stderr)
	return response.ExitCode, true
}

func normalizeWorkerPathArgs(args []string) ([]string, error) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolve caller working directory: %w", err)
	}
	return normalizeWorkerPathArgsFrom(args, workingDirectory)
}

func normalizeWorkerPathArgsFrom(args []string, workingDirectory string) ([]string, error) {
	result := append([]string(nil), args...)
	absolute := func(value string) (string, error) {
		if value == "" || filepath.IsAbs(value) {
			return value, nil
		}
		if workingDirectory == "" || !filepath.IsAbs(workingDirectory) {
			return "", errors.New("caller working directory must be absolute")
		}
		return filepath.Clean(filepath.Join(workingDirectory, value)), nil
	}
	for index := 0; index < len(result); index++ {
		argument := result[index]
		if argument == "--" {
			break
		}
		if workerPathFlag(argument) {
			if index+1 >= len(result) {
				return nil, fmt.Errorf("%s requires a path", argument)
			}
			index++
			value, err := absolute(result[index])
			if err != nil {
				return nil, err
			}
			result[index] = value
			continue
		}
		if flag, value, ok := strings.Cut(argument, "="); ok && workerPathFlag(flag) {
			value, err := absolute(value)
			if err != nil {
				return nil, err
			}
			result[index] = flag + "=" + value
			continue
		}
		if workerFlagTakesValue(argument) && index+1 < len(result) {
			// Keep a non-path flag's following value opaque, even when that
			// value happens to look like a path flag.
			index++
		}
	}
	return result, nil
}

func workerPathFlag(argument string) bool {
	switch argument {
	case "--images-file", "--output-dir", "--photo-ids-file", "--file", "--files-file", "--dates-file",
		"-images-file", "-output-dir", "-photo-ids-file", "-file", "-files-file", "-dates-file":
		return true
	default:
		return false
	}
}

func workerFlagTakesValue(argument string) bool {
	if commandFlagTakesValue(argument) {
		return true
	}
	if strings.HasPrefix(argument, "-") && !strings.HasPrefix(argument, "--") {
		return commandFlagTakesValue("-" + argument)
	}
	return false
}

func validateWorkerPathsAbsolute(args []string) error {
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			break
		}
		if workerPathFlag(argument) {
			if index+1 >= len(args) {
				return fmt.Errorf("%s requires a path", argument)
			}
			index++
			if args[index] != "" && !filepath.IsAbs(args[index]) {
				return fmt.Errorf("%s must be absolute when sent to the worker", argument)
			}
			continue
		}
		if flag, value, ok := strings.Cut(argument, "="); ok && workerPathFlag(flag) {
			if value != "" && !filepath.IsAbs(value) {
				return fmt.Errorf("%s must be absolute when sent to the worker", flag)
			}
			continue
		}
		if workerFlagTakesValue(argument) && index+1 < len(args) {
			index++
		}
	}
	return nil
}

func classifyWorkerCommand(args []string) (workerCommandDisposition, error) {
	normalized, _, err := parseGlobals(args)
	if err != nil {
		return workerCommandReject, err
	}
	if len(normalized) == 0 || normalized[0] == "help" || normalized[0] == "--help" ||
		normalized[0] == "-h" || normalized[0] == "version" || normalized[0] == "--version" {
		return workerCommandLocal, nil
	}
	if len(normalized) < 2 {
		return workerCommandReject, errors.New("worker accepts only an existing auth, albums, or photos subcommand")
	}
	switch normalized[0] {
	case "auth":
		switch normalized[1] {
		case "status", "photos":
			return workerCommandForward, nil
		}
	case "albums":
		switch normalized[1] {
		case "list", "get", "create", "add", "import", "duplicates", "remove":
			return workerCommandForward, nil
		}
	case "photos":
		switch normalized[1] {
		case "hide", "unhide", "hidden", "set-date":
			return workerCommandForward, nil
		}
	}
	return workerCommandReject, fmt.Errorf("command %q is not allowed through the Photos worker", strings.Join(normalized[:2], " "))
}

func workerCommandMutates(args []string) bool {
	normalized, _, err := parseGlobals(args)
	if err != nil || len(normalized) < 2 {
		return false
	}
	if normalized[0] == "albums" {
		switch normalized[1] {
		case "create", "add", "import", "remove":
		default:
			return false
		}
	} else if normalized[0] == "photos" {
		switch normalized[1] {
		case "hide", "unhide", "set-date":
		default:
			return false
		}
	} else {
		return false
	}
	execute := false
	rest := normalized[2:]
	for index := 0; index < len(rest); index++ {
		arg := rest[index]
		if arg == "--" {
			break
		}
		// A flag that takes a separate value consumes it, so a value that
		// looks like --execute=false is never read as a flag.
		if !strings.Contains(arg, "=") && workerFlagTakesValue(arg) {
			index++
			continue
		}
		switch arg {
		case "--execute", "-execute":
			execute = true
		default:
			flag, value, ok := strings.Cut(arg, "=")
			if !ok || (flag != "--execute" && flag != "-execute") {
				continue
			}
			if parsed, err := strconv.ParseBool(value); err == nil {
				execute = parsed
			}
		}
	}
	return execute
}

func workerIsConfigured(paths workerPaths) (bool, error) {
	info, err := os.Lstat(paths.marker)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, fmt.Errorf("inspect worker marker: %w", err)
	}
	if err := validatePrivateDirectory(paths.directory, false); err != nil {
		return true, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return true, errors.New("worker marker is not a regular file")
	}
	if err := validateOwned(info, "worker marker"); err != nil {
		return true, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return true, errors.New("worker marker is accessible to other users")
	}
	return true, nil
}

func validateWorkerSocket(paths workerPaths) error {
	if err := validatePrivateDirectory(paths.directory, false); err != nil {
		return err
	}
	info, err := os.Lstat(paths.socket)
	if err != nil {
		return fmt.Errorf("inspect worker socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("worker socket path is not a Unix socket")
	}
	if err := validateOwned(info, "worker socket"); err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("worker socket is accessible to other users")
	}
	return nil
}

func serveWorker(ctx context.Context, paths workerPaths, runner workerRunner) error {
	if err := validatePrivateDirectory(paths.directory, true); err != nil {
		return err
	}
	lock, err := acquireWorkerLock(paths.directory)
	if err != nil {
		return err
	}
	// The lock outlives the socket: a stopping worker keeps it until its
	// last request has finished, so a replacement can never run beside it.
	defer lock.Close()
	listener, ownedInfo, err := prepareWorkerSocket(paths)
	if err != nil {
		return err
	}
	defer func() {
		_ = listener.Close()
		cleanupOwnedSocket(paths.socket, ownedInfo)
	}()

	closed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-closed:
		}
	}()
	defer close(closed)

	for {
		connection, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept worker connection: %w", err)
		}
		handleWorkerConnection(connection, runner)
	}
}

// acquireWorkerLock takes an exclusive flock that the kernel releases when
// the worker process ends, however it ends.
func acquireWorkerLock(directory string) (*os.File, error) {
	path := filepath.Join(directory, workerLockFilename)
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open worker lock: %w", err)
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("worker lock is not a regular file")
	}
	if err == nil {
		err = validateOwned(info, "worker lock")
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another photoscli worker is still running")
		}
		return nil, fmt.Errorf("lock worker: %w", err)
	}
	return file, nil
}

func prepareWorkerSocket(paths workerPaths) (*net.UnixListener, os.FileInfo, error) {
	if err := validatePrivateDirectory(paths.directory, true); err != nil {
		return nil, nil, err
	}
	if err := removeOwnedStaleSocket(paths.socket); err != nil {
		return nil, nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: paths.socket, Net: "unix"})
	if err != nil {
		return nil, nil, fmt.Errorf("listen on worker socket: %w", err)
	}
	listener.SetUnlinkOnClose(false)
	info, err := os.Lstat(paths.socket)
	if err != nil {
		_ = listener.Close()
		return nil, nil, fmt.Errorf("inspect new worker socket: %w", err)
	}
	if err := validateOwned(info, "worker socket"); err != nil {
		_ = listener.Close()
		return nil, nil, err
	}
	if err := os.Chmod(paths.socket, 0o600); err != nil {
		_ = listener.Close()
		cleanupOwnedSocket(paths.socket, info)
		return nil, nil, fmt.Errorf("protect worker socket: %w", err)
	}
	return listener, info, nil
}

func validatePrivateDirectory(directory string, create bool) error {
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) && create {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("create worker directory: %w", err)
		}
		info, err = os.Lstat(directory)
	}
	if err != nil {
		return fmt.Errorf("inspect worker directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("worker directory is not a real directory")
	}
	if err := validateOwned(info, "worker directory"); err != nil {
		return err
	}
	if create && info.Mode().Perm() != 0o700 {
		if err := os.Chmod(directory, 0o700); err != nil {
			return fmt.Errorf("protect worker directory: %w", err)
		}
	} else if !create && info.Mode().Perm()&0o077 != 0 {
		return errors.New("worker directory is accessible to other users")
	}
	return nil
}

func validateOwned(info os.FileInfo, label string) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("could not determine %s ownership", label)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%s is owned by a different user", label)
	}
	return nil
}

func removeOwnedStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect existing worker socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("worker socket path already exists and is not a Unix socket")
	}
	if err := validateOwned(info, "existing worker socket"); err != nil {
		return err
	}
	connection, dialErr := net.DialTimeout("unix", path, 250*time.Millisecond)
	if dialErr == nil {
		_ = connection.Close()
		return errors.New("another photoscli worker already owns the socket")
	}
	// Only a refused connection proves no worker is listening. A timeout or
	// a full backlog means a live, busy worker; removing its socket would let
	// a second worker run beside it.
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		return fmt.Errorf("worker socket did not refuse the connection, so a worker may still own it: %w", dialErr)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("recheck stale worker socket: %w", err)
	}
	if !os.SameFile(info, current) || current.Mode()&os.ModeSocket == 0 {
		return errors.New("worker socket changed while checking its owner")
	}
	if err := validateOwned(current, "stale worker socket"); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale worker socket: %w", err)
	}
	return nil
}

func cleanupOwnedSocket(path string, owned os.FileInfo) {
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(owned, current) || current.Mode()&os.ModeSocket == 0 {
		return
	}
	if validateOwned(current, "worker socket") != nil {
		return
	}
	_ = os.Remove(path)
}

func handleWorkerConnection(connection *net.UnixConn, runner workerRunner) {
	defer connection.Close()
	respond := func(response workerResponse) {
		_ = connection.SetWriteDeadline(time.Now().Add(workerIdleTimeout))
		writeWorkerResponse(connection, response)
	}
	_ = connection.SetDeadline(time.Now().Add(workerIdleTimeout))
	payload, err := readBounded(connection, maxWorkerRequestBytes)
	if err != nil {
		respond(workerResponseError("worker_bad_request", err.Error(), false))
		return
	}
	request, err := decodeWorkerRequest(payload)
	if err != nil {
		respond(workerResponseError("worker_bad_request", err.Error(), false))
		return
	}
	if disposition, err := classifyWorkerCommand(request.Args); disposition != workerCommandForward {
		if err == nil {
			err = errors.New("command is not allowed through the Photos worker")
		}
		respond(workerResponseError("worker_command_rejected", err.Error(), false))
		return
	}
	if err := validateWorkerPathsAbsolute(request.Args); err != nil {
		respond(workerResponseError("worker_bad_request", err.Error(), false))
		return
	}
	if len(request.Args) > 0 && workerFlagTakesValue(request.Args[len(request.Args)-1]) {
		respond(workerResponseError("worker_bad_request",
			request.Args[len(request.Args)-1]+" requires a value", false))
		return
	}
	deadline := time.UnixMilli(request.DeadlineUnixMillis)
	remaining := time.Until(deadline)
	if remaining <= 0 {
		respond(workerResponseError("worker_request_expired",
			"worker request expired in the queue before execution", false))
		return
	}
	_ = connection.SetDeadline(deadline.Add(workerResultGrace))
	args := append(append([]string(nil), request.Args...), "--timeout="+remaining.String())
	response := executeWorkerRequest(args, runner)
	respond(response)
}

func executeWorkerRequest(args []string, runner workerRunner) (response workerResponse) {
	response.Version = workerProtocolVersion
	mutates := workerCommandMutates(args)
	defer func() {
		if recovered := recover(); recovered != nil {
			response = workerResponseError("worker_runner_panic", "the worker command stopped unexpectedly", mutates)
		}
	}()
	stdout := newCappedBuffer(maxWorkerOutputBytes)
	stderr := newCappedBuffer(maxWorkerOutputBytes)
	response.ExitCode = runner(append([]string(nil), args...), stdout, stderr)
	if stdout.overflow || stderr.overflow {
		return workerResponseError("worker_output_too_large", "worker output exceeded the response limit", mutates)
	}
	response.Stdout = stdout.String()
	response.Stderr = stderr.String()
	return response
}

func decodeWorkerRequest(payload []byte) (workerRequest, error) {
	var request workerRequest
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, fmt.Errorf("decode worker request: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return request, err
	}
	if request.Version != workerProtocolVersion {
		return request, fmt.Errorf("unsupported worker protocol version %d", request.Version)
	}
	if request.DeadlineUnixMillis <= 0 {
		return request, errors.New("worker request has no valid deadline")
	}
	if request.Args == nil || len(request.Args) > maxWorkerArgumentCount {
		return request, errors.New("worker request has an invalid argument count")
	}
	for _, argument := range request.Args {
		if strings.IndexByte(argument, 0) >= 0 {
			return request, errors.New("worker arguments must not contain NUL bytes")
		}
	}
	return request, nil
}

func decodeWorkerResponse(payload []byte) (workerResponse, error) {
	var response workerResponse
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return response, fmt.Errorf("decode worker response: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return response, err
	}
	if response.Version != workerProtocolVersion {
		return response, fmt.Errorf("unsupported worker protocol version %d", response.Version)
	}
	return response, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("worker frame contains more than one JSON value")
		}
		return fmt.Errorf("decode trailing worker data: %w", err)
	}
	return nil
}

func callWorker(socket string, args []string) (workerResponse, bool, error) {
	_, timeout, parseErr := parseGlobals(args)
	if parseErr != nil {
		return workerResponse{}, false, parseErr
	}
	deadline := time.Now().Add(timeout)
	requestPayload, err := json.Marshal(workerRequest{
		Version:            workerProtocolVersion,
		DeadlineUnixMillis: deadline.UnixMilli(),
		Args:               args,
	})
	if err != nil {
		return workerResponse{}, false, fmt.Errorf("encode worker request: %w", err)
	}
	if len(requestPayload) > maxWorkerRequestBytes {
		return workerResponse{}, false, errors.New("worker request exceeds the size limit")
	}
	connection, err := net.DialTimeout("unix", socket, workerDialTimeout)
	if err != nil {
		return workerResponse{}, false, fmt.Errorf("connect to configured Photos worker: %w", err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(deadline.Add(workerResultGrace))
	written, err := io.Copy(connection, bytes.NewReader(requestPayload))
	submitted := written == int64(len(requestPayload))
	if err != nil || !submitted {
		if err == nil {
			err = io.ErrShortWrite
		}
		return workerResponse{}, submitted, fmt.Errorf("submit worker request: %w", err)
	}
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return workerResponse{}, submitted, errors.New("worker connection is not a Unix socket")
	}
	if err := unixConnection.CloseWrite(); err != nil {
		return workerResponse{}, submitted, fmt.Errorf("finish worker request: %w", err)
	}
	responsePayload, err := readBounded(connection, maxWorkerResponseBytes)
	if err != nil {
		return workerResponse{}, submitted, fmt.Errorf("read worker response: %w", err)
	}
	response, err := decodeWorkerResponse(responsePayload)
	if err != nil {
		return workerResponse{}, submitted, err
	}
	return response, submitted, nil
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("worker frame exceeds the size limit")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("worker frame is empty")
	}
	return data, nil
}

func writeWorkerResponse(writer io.Writer, response workerResponse) {
	_ = json.NewEncoder(writer).Encode(response)
}

func workerResponseError(code, message string, readbackRequired bool) workerResponse {
	return workerResponse{
		Version:  workerProtocolVersion,
		ExitCode: 1,
		Error: &workerProtocolError{
			Code:             code,
			Message:          message,
			ReadbackRequired: readbackRequired,
		},
	}
}

func writeWorkerError(stderr io.Writer, code, message string, readbackRequired bool) int {
	details := map[string]any(nil)
	if readbackRequired {
		details = map[string]any{"readbackRequired": true}
	}
	return writeError(stderr, "worker", &app.CodedError{Code: code, Message: message, Details: details})
}

type cappedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func newCappedBuffer(limit int) *cappedBuffer {
	return &cappedBuffer{limit: limit}
}

func (buffer *cappedBuffer) Write(data []byte) (int, error) {
	available := buffer.limit - buffer.buffer.Len()
	if available <= 0 {
		buffer.overflow = true
		return len(data), nil
	}
	if len(data) > available {
		_, _ = buffer.buffer.Write(data[:available])
		buffer.overflow = true
		return len(data), nil
	}
	_, _ = buffer.buffer.Write(data)
	return len(data), nil
}

func (buffer *cappedBuffer) String() string {
	return buffer.buffer.String()
}
