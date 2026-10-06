package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbelinky/photoscli/internal/app"
)

const version = "0.3.0"

type outputEnvelope struct {
	OK      bool            `json:"ok"`
	Command string          `json:"command"`
	Result  any             `json:"result,omitempty"`
	Error   *app.CodedError `json:"error,omitempty"`
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "worker" {
		os.Exit(runWorker(args[1:], os.Stdout, os.Stderr))
	}
	if code, handled := forwardToWorker(args, os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	os.Exit(run(args, os.Stdout, os.Stderr, app.OsaBackend{}))
}

func run(args []string, stdout, stderr io.Writer, backend app.Backend) int {
	args, timeout, globalErr := parseGlobals(args)
	if globalErr != nil {
		return writeError(stderr, "", globalErr)
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if args[0] == "version" || args[0] == "--version" {
		writeJSON(stdout, outputEnvelope{OK: true, Command: "version", Result: map[string]string{"version": version}})
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	service := app.NewService(backend)

	switch args[0] {
	case "auth":
		return runAuth(ctx, args[1:], stdout, stderr, backend)
	case "albums":
		return runAlbums(ctx, args[1:], stdout, stderr, service, backend)
	case "photos":
		return runPhotos(ctx, args[1:], stdout, stderr)
	default:
		return writeError(stderr, "", usageError("unknown command %q", args[0]))
	}
}

func runAuth(ctx context.Context, args []string, stdout, stderr io.Writer, backend app.Backend) int {
	if len(args) > 0 && args[0] == "photos" {
		return runPhotosAuth(ctx, args[1:], stdout, stderr)
	}
	if len(args) == 0 || args[0] != "status" {
		return writeError(stderr, "auth", usageError("expected: auth status [--probe]"))
	}
	flags := newFlagSet("auth status")
	probe := flags.Bool("probe", false, "send a read-only event to Photos")
	if err := flags.Parse(args[1:]); err != nil {
		return flagError(stdout, stderr, "auth.status", flags, err)
	}
	if flags.NArg() != 0 {
		return writeError(stderr, "auth.status", usageError("unexpected arguments: %s", strings.Join(flags.Args(), " ")))
	}
	if !*probe {
		result := map[string]any{
			"automation": "unknown",
			"probed":     false,
			"reason":     "macOS Automation permission cannot be determined reliably without sending an Apple event; rerun with --probe",
		}
		writeJSON(stdout, outputEnvelope{OK: true, Command: "auth.status", Result: result})
		return 0
	}
	count, err := backend.Probe(ctx)
	if err != nil {
		return writeError(stderr, "auth.status", err)
	}
	writeJSON(stdout, outputEnvelope{OK: true, Command: "auth.status", Result: map[string]any{"automation": "granted", "probed": true, "albumCount": count}})
	return 0
}

func runAlbums(ctx context.Context, args []string, stdout, stderr io.Writer, service *app.Service, backend app.Backend) int {
	if len(args) == 0 {
		return writeError(stderr, "albums", usageError("expected one of: list, get, create, add, import, duplicates, remove"))
	}
	switch args[0] {
	case "duplicates":
		return runAlbumDuplicates(ctx, args[1:], stdout, stderr, service, backend)
	case "remove":
		return runAlbumRemove(ctx, args[1:], stdout, stderr)
	case "list":
		flags := newFlagSet("albums list")
		name := flags.String("name", "", "exact album name")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return flagError(stdout, stderr, "albums.list", flags, err)
		}
		albums, err := service.ListAlbums(ctx, *name)
		if err != nil {
			return writeError(stderr, "albums.list", err)
		}
		writeJSON(stdout, outputEnvelope{OK: true, Command: "albums.list", Result: map[string]any{"albums": albums, "count": len(albums)}})
		return 0
	case "get":
		flags := newFlagSet("albums get")
		id := flags.String("id", "", "exact Photos album ID")
		name := flags.String("name", "", "exact album name")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return flagError(stdout, stderr, "albums.get", flags, err)
		}
		album, err := service.GetAlbum(ctx, *id, *name)
		if err != nil {
			return writeError(stderr, "albums.get", err)
		}
		writeJSON(stdout, outputEnvelope{OK: true, Command: "albums.get", Result: album})
		return 0
	case "create":
		flags := newFlagSet("albums create")
		name := flags.String("name", "", "album name")
		execute := flags.Bool("execute", false, "apply the change")
		dryRun := flags.Bool("dry-run", false, "show the change without applying it")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return flagError(stdout, stderr, "albums.create", flags, err)
		}
		if *execute && *dryRun {
			return writeError(stderr, "albums.create", usageError("--execute and --dry-run cannot be used together"))
		}
		result, err := service.CreateAlbum(ctx, *name, *execute)
		if err != nil {
			return writeError(stderr, "albums.create", err)
		}
		writeJSON(stdout, outputEnvelope{OK: true, Command: "albums.create", Result: result})
		return 0
	case "add":
		flags := newFlagSet("albums add")
		albumID := flags.String("album-id", "", "exact Photos album ID")
		var ids stringList
		flags.Var(&ids, "photo-id", "exact Photos media-item ID (repeatable)")
		idsFile := flags.String("photo-ids-file", "", "newline-delimited or JSON-array photo IDs")
		execute := flags.Bool("execute", false, "apply the change")
		dryRun := flags.Bool("dry-run", false, "show the change without applying it")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return flagError(stdout, stderr, "albums.add", flags, err)
		}
		if *execute && *dryRun {
			return writeError(stderr, "albums.add", usageError("--execute and --dry-run cannot be used together"))
		}
		if *idsFile != "" {
			fileIDs, err := readIDs(*idsFile)
			if err != nil {
				return writeError(stderr, "albums.add", &app.CodedError{Code: "input_error", Message: err.Error()})
			}
			ids = append(ids, fileIDs...)
		}
		result, err := service.AddPhotos(ctx, *albumID, ids, *execute)
		if err != nil {
			return writeError(stderr, "albums.add", err)
		}
		writeJSON(stdout, outputEnvelope{OK: true, Command: "albums.add", Result: result})
		return 0
	case "import":
		flags := newFlagSet("albums import")
		albumID := flags.String("album-id", "", "exact Photos album ID")
		var paths stringList
		flags.Var(&paths, "file", "path to a photo or video file (repeatable)")
		filesFile := flags.String("files-file", "", "JSON array of file paths")
		skipDuplicateCheck := flags.Bool("skip-duplicate-check", false, "import without Photos duplicate checking; dedupe first")
		execute := flags.Bool("execute", false, "apply the change")
		dryRun := flags.Bool("dry-run", false, "show the change without applying it")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return flagError(stdout, stderr, "albums.import", flags, err)
		}
		if *execute && *dryRun {
			return writeError(stderr, "albums.import", usageError("--execute and --dry-run cannot be used together"))
		}
		if *filesFile != "" {
			filePaths, err := readFiles(*filesFile)
			if err != nil {
				return writeError(stderr, "albums.import", &app.CodedError{Code: "input_error", Message: err.Error()})
			}
			manifestPath, err := filepath.Abs(*filesFile)
			if err != nil {
				return writeError(stderr, "albums.import", err)
			}
			for i := range filePaths {
				if filePaths[i] != "" && !filepath.IsAbs(filePaths[i]) {
					filePaths[i] = filepath.Join(filepath.Dir(manifestPath), filePaths[i])
				}
			}
			paths = append(paths, filePaths...)
		}
		result, err := service.ImportFiles(ctx, *albumID, paths, *skipDuplicateCheck, *execute)
		if err != nil {
			return writeError(stderr, "albums.import", err)
		}
		writeJSON(stdout, outputEnvelope{OK: true, Command: "albums.import", Result: result})
		return 0
	default:
		return writeError(stderr, "albums", usageError("unknown albums command %q", args[0]))
	}
}

func parseGlobals(args []string) ([]string, time.Duration, error) {
	timeout := 60 * time.Second
	result := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if commandFlagTakesValue(arg) {
			result = append(result, arg)
			if i+1 < len(args) {
				i++
				result = append(result, args[i])
			}
			continue
		}
		switch {
		case arg == "--json" || arg == "--no-input":
			continue
		case arg == "--timeout":
			if i+1 >= len(args) {
				return nil, 0, usageError("--timeout requires a duration")
			}
			i++
			parsed, err := time.ParseDuration(args[i])
			if err != nil || parsed <= 0 {
				return nil, 0, usageError("invalid --timeout duration %q", args[i])
			}
			timeout = parsed
		case strings.HasPrefix(arg, "--timeout="):
			value := strings.TrimPrefix(arg, "--timeout=")
			parsed, err := time.ParseDuration(value)
			if err != nil || parsed <= 0 {
				return nil, 0, usageError("invalid --timeout duration %q", value)
			}
			timeout = parsed
		default:
			result = append(result, arg)
		}
	}
	return result, timeout, nil
}

func commandFlagTakesValue(arg string) bool {
	if !strings.HasPrefix(arg, "-") {
		return false
	}
	switch strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-") {
	case "name", "id", "album-id", "photo-id", "photo-ids-file", "file", "files-file", "images-file", "output-dir", "distance", "reason", "batch", "since", "dates-file":
		return true
	default:
		return false
	}
}

func readFiles(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read file paths: %w", err)
	}
	var paths []string
	if err := json.Unmarshal(data, &paths); err != nil {
		return nil, fmt.Errorf("parse JSON file paths: %w", err)
	}
	return paths, nil
}

func readIDs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read photo IDs: %w", err)
	}
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "[") {
		var ids []string
		if err := json.Unmarshal(data, &ids); err != nil {
			return nil, fmt.Errorf("parse JSON photo IDs: %w", err)
		}
		return ids, nil
	}
	var ids []string
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			ids = append(ids, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read photo IDs: %w", err)
	}
	return ids, nil
}

func newFlagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func flagError(stdout, stderr io.Writer, command string, flags *flag.FlagSet, parseErr error) int {
	if errors.Is(parseErr, flag.ErrHelp) {
		fmt.Fprintln(stdout, commandUsage(command))
		return 0
	}
	if parseErr != nil {
		return writeError(stderr, command, usageError("%v", parseErr))
	}
	return writeError(stderr, command, usageError("unexpected arguments: %s", strings.Join(flags.Args(), " ")))
}

func commandUsage(command string) string {
	switch command {
	case "auth.status":
		return "Usage: photoscli auth status [--probe] [--timeout DURATION]"
	case "auth.photos":
		return "Usage: photoscli auth photos [--request] [--timeout DURATION]"
	case "albums.duplicates":
		return "Usage: photoscli albums duplicates --album-id ID [--images-file FILE | --output-dir DIR] [--distance 12] [--timeout DURATION]"
	case "albums.remove":
		return "Usage: photoscli albums remove --album-id ID (--photo-id ID ... | --photo-ids-file FILE) [--execute | --dry-run] [--timeout DURATION]"
	case "albums.list":
		return "Usage: photoscli albums list [--name NAME] [--timeout DURATION]"
	case "albums.get":
		return "Usage: photoscli albums get (--id ID | --name NAME) [--timeout DURATION]"
	case "albums.create":
		return "Usage: photoscli albums create --name NAME [--execute | --dry-run] [--timeout DURATION]"
	case "albums.add":
		return "Usage: photoscli albums add --album-id ID (--photo-id ID ... | --photo-ids-file FILE) [--execute | --dry-run] [--timeout DURATION]"
	case "albums.import":
		return "Usage: photoscli albums import --album-id ID (--file PATH ... | --files-file FILE) [--skip-duplicate-check] [--execute | --dry-run] [--timeout DURATION]"
	case "photos.hide":
		return "Usage: photoscli photos hide (--photo-id ID ... | --photo-ids-file FILE) --reason TEXT [--batch LABEL] [--execute | --dry-run] [--timeout DURATION]"
	case "photos.unhide":
		return "Usage: photoscli photos unhide (--photo-id ID ... | --photo-ids-file FILE) --reason TEXT [--batch LABEL] [--execute | --dry-run] [--timeout DURATION]"
	case "photos.hidden.list":
		return "Usage: photoscli photos hidden list [--reason TEXT] [--since DURATION] [--ids-only] [--timeout DURATION]"
	default:
		return usage
	}
}

func usageError(format string, values ...any) error {
	return &app.CodedError{Code: "usage", Message: fmt.Sprintf(format, values...)}
}

func writeError(writer io.Writer, command string, err error) int {
	var coded *app.CodedError
	if !errors.As(err, &coded) {
		coded = &app.CodedError{Code: "internal", Message: err.Error()}
	}
	writeJSON(writer, outputEnvelope{OK: false, Command: command, Error: coded})
	switch coded.Code {
	case "usage", "input_error":
		return 2
	case "not_found", "photos_not_found":
		return 5
	case "automation_denied":
		return 6
	case "timeout":
		return 8
	default:
		return 1
	}
}

func writeJSON(writer io.Writer, value any) {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

const usage = `photoscli controls Apple Photos albums through native macOS interfaces.

Usage:
  photoscli auth status [--probe]
  photoscli auth photos [--request]
  photoscli albums list [--name NAME]
  photoscli albums get (--id ID | --name NAME)
  photoscli albums create --name NAME [--execute]
  photoscli albums add --album-id ID (--photo-id ID ... | --photo-ids-file FILE) [--execute]
  photoscli albums import --album-id ID (--file PATH ... | --files-file FILE) [--skip-duplicate-check] [--execute]
  photoscli albums duplicates --album-id ID [--images-file FILE | --output-dir DIR]
  photoscli albums remove --album-id ID (--photo-id ID ... | --photo-ids-file FILE) [--execute]
  photoscli photos hide (--photo-id ID ... | --photo-ids-file FILE) --reason TEXT [--batch LABEL] [--execute]
  photoscli photos unhide (--photo-id ID ... | --photo-ids-file FILE) --reason TEXT [--batch LABEL] [--execute]
  photoscli photos hidden list [--reason TEXT] [--since DURATION] [--ids-only]

All output is JSON. Mutations are dry-runs unless --execute is present.
Remove changes album membership only; photos remain in the library.
Global flags: --json, --no-input, --timeout DURATION
`
