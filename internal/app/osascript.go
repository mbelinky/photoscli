package app

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
)

//go:embed bridge.js
var bridgeScript string

//go:embed add.applescript
var addScript string

//go:embed import.applescript
var importScript string

type OsaBackend struct{}

var appleScriptRunner = runAppleScript

type bridgeEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *CodedError     `json:"error"`
}

func (OsaBackend) call(ctx context.Context, operation string, input any, output any) error {
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", "-l", "JavaScript", "-", operation, string(payload))
	cmd.Stdin = strings.NewReader(bridgeScript)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return timeoutError(operation)
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		if strings.Contains(message, "-1743") || strings.Contains(strings.ToLower(message), "not authorized") {
			return errCode("automation_denied", "macOS denied Automation access to Photos", map[string]any{"cause": message})
		}
		return errCode("bridge_failed", "could not run the Photos automation bridge", map[string]any{"cause": message})
	}
	var envelope bridgeEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		return errCode("bridge_failed", "Photos returned an unreadable response", map[string]any{"cause": err.Error()})
	}
	if !envelope.OK {
		if envelope.Error != nil {
			if envelope.Error.Code == "timeout" && isMutation(operation) {
				return timeoutError(operation)
			}
			return envelope.Error
		}
		return errCode("photos_error", "Photos returned an unknown error", nil)
	}
	if output == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, output); err != nil {
		return errCode("bridge_failed", "Photos returned an unexpected response", map[string]any{"cause": err.Error()})
	}
	return nil
}

func isMutation(operation string) bool {
	return operation == "albums.create" || operation == "albums.add" || operation == "albums.import"
}

func timeoutError(operation string) error {
	if isMutation(operation) {
		return errCode("timeout", "Photos timed out and the mutation outcome is unknown; inspect the album before retrying", map[string]any{"operation": operation, "outcome": "unknown"})
	}
	return errCode("timeout", "Photos did not respond before the timeout", map[string]any{"operation": operation})
}

func (b OsaBackend) ListAlbums(ctx context.Context) ([]Album, error) {
	var albums []Album
	err := b.call(ctx, "albums.list", struct{}{}, &albums)
	return albums, err
}

func (b OsaBackend) GetAlbum(ctx context.Context, id string) (Album, error) {
	var album Album
	err := b.call(ctx, "albums.get", map[string]string{"id": id}, &album)
	return album, err
}

func (b OsaBackend) FindPhotoIDs(ctx context.Context, ids []string) ([]string, error) {
	var found []string
	err := b.call(ctx, "photos.find", map[string][]string{"ids": ids}, &found)
	return found, err
}

func (b OsaBackend) CreateAlbum(ctx context.Context, name string) (Album, error) {
	var album Album
	err := b.call(ctx, "albums.create", map[string]string{"name": name}, &album)
	return album, err
}

func (b OsaBackend) AddPhotos(ctx context.Context, albumID string, ids []string) error {
	args := append([]string{albumID}, ids...)
	_, err := runAppleScript(ctx, "albums.add", addScript, args)
	return err
}

func (b OsaBackend) ImportFiles(ctx context.Context, albumID string, paths []string, skipDuplicateCheck bool) ([]string, error) {
	skip := "false"
	if skipDuplicateCheck {
		skip = "true"
	}
	args := append([]string{skip, albumID}, paths...)
	output, err := appleScriptRunner(ctx, "albums.import", importScript, args)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return []string{}, nil
	}
	return strings.Split(trimmed, "\n"), nil
}

func runAppleScript(ctx context.Context, operation, script string, arguments []string) (string, error) {
	args := append([]string{"-"}, arguments...)
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", args...)
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || strings.Contains(stderr.String(), "-1712") {
			return "", timeoutError(operation)
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		if strings.Contains(message, "-1743") || strings.Contains(strings.ToLower(message), "not authorized") {
			return "", errCode("automation_denied", "macOS denied Automation access to Photos", map[string]any{"cause": message})
		}
		details := map[string]any{"cause": message, "operation": operation}
		if isMutation(operation) {
			details["outcome"] = "unknown"
		}
		return "", errCode("photos_error", "Photos could not complete the requested operation", details)
	}
	return stdout.String(), nil
}

func (b OsaBackend) Probe(ctx context.Context) (int, error) {
	var result struct {
		AlbumCount int `json:"albumCount"`
	}
	err := b.call(ctx, "auth.probe", struct{}{}, &result)
	return result.AlbumCount, err
}
