package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/mbelinky/photoscli/internal/app"
	"github.com/mbelinky/photoscli/internal/ledger"
	"github.com/mbelinky/photoscli/internal/photokit"
)

func runPhotos(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return writeError(stderr, "photos", usageError("expected one of: hide, unhide, hidden, set-date"))
	}
	switch args[0] {
	case "hide":
		return runSetHidden(ctx, args[1:], true, stdout, stderr)
	case "unhide":
		return runSetHidden(ctx, args[1:], false, stdout, stderr)
	case "hidden":
		return runHidden(ctx, args[1:], stdout, stderr)
	case "set-date":
		return runSetDate(ctx, args[1:], stdout, stderr)
	default:
		return writeError(stderr, "photos", usageError("unknown photos command %q", args[0]))
	}
}

func runSetHidden(ctx context.Context, args []string, hidden bool, stdout, stderr io.Writer) int {
	command, action := "photos.hide", "hide"
	if !hidden {
		command, action = "photos.unhide", "unhide"
	}
	flags := newFlagSet(action)
	var ids stringList
	flags.Var(&ids, "photo-id", "exact Photos media-item ID (repeatable)")
	idsFile := flags.String("photo-ids-file", "", "newline-delimited or JSON-array photo IDs")
	reason := flags.String("reason", "", "reason for this curation decision")
	batch := flags.String("batch", "", "optional curation batch label")
	execute := flags.Bool("execute", false, "apply the hidden-state change")
	dryRun := flags.Bool("dry-run", false, "preview the hidden-state change")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return flagError(stdout, stderr, command, flags, err)
	}
	if *execute && *dryRun {
		return writeError(stderr, command, usageError("--execute and --dry-run cannot be used together"))
	}
	if *reason == "" {
		return writeError(stderr, command, usageError("--reason is required"))
	}
	if *idsFile != "" {
		fileIDs, err := readIDs(*idsFile)
		if err != nil {
			return writeError(stderr, command, &app.CodedError{Code: "input_error", Message: err.Error()})
		}
		ids = append(ids, fileIDs...)
	}
	result, err := photokit.SetHidden(ctx, ids, hidden, *execute)
	if *execute && result.Executed && (result.Verified || result.OutcomeUnknown) {
		if ledgerErr := appendCurationLedger(action, *reason, *batch, result); ledgerErr != nil {
			return writeError(stderr, command, &app.CodedError{Code: "ledger_error", Message: ledgerErr.Error(), Details: map[string]any{"result": result}})
		}
	}
	if err != nil {
		coded := photosError(err)
		if coded.Details == nil {
			coded.Details = make(map[string]any)
		}
		coded.Details["result"] = result
		return writeError(stderr, command, coded)
	}
	writeJSON(stdout, outputEnvelope{OK: true, Command: command, Result: result})
	return 0
}

func appendCurationLedger(action, reason, batch string, result photokit.SetHiddenResult) error {
	path, err := curationLedgerPath()
	if err != nil {
		return err
	}
	changed := make(map[string]bool, len(result.ChangedPhotoIDs))
	for _, id := range result.ChangedPhotoIDs {
		changed[id] = true
	}
	entries := make([]ledger.Entry, 0, len(result.RequestedPhotoIDs))
	now := time.Now().UTC()
	for _, id := range result.RequestedPhotoIDs {
		entries = append(entries, ledger.Entry{At: now, Action: action, PhotoID: id, Reason: reason, Batch: batch, Changed: changed[id], Verified: result.Verified})
	}
	return ledger.Append(path, entries)
}

func curationLedgerPath() (string, error) {
	directory, _, _, err := workerServicePaths()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "curation-ledger.jsonl"), nil
}

func runHidden(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "list" {
		return writeError(stderr, "photos.hidden", usageError("expected: photos hidden list"))
	}
	flags := newFlagSet("photos hidden list")
	reason := flags.String("reason", "", "exact curation reason")
	sinceText := flags.String("since", "", "only hides from this long ago, such as 72h or 30d")
	idsOnly := flags.Bool("ids-only", false, "write a JSON array of photo IDs")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		return flagError(stdout, stderr, "photos.hidden.list", flags, err)
	}
	since, err := ledger.ParseSince(*sinceText)
	if err != nil {
		return writeError(stderr, "photos.hidden.list", usageError("--since: %v", err))
	}
	if err := ctx.Err(); err != nil {
		return writeError(stderr, "photos.hidden.list", err)
	}
	path, err := curationLedgerPath()
	if err != nil {
		return writeError(stderr, "photos.hidden.list", err)
	}
	result, err := ledger.ReadHidden(path, *reason, since, time.Now().UTC())
	if err != nil {
		return writeError(stderr, "photos.hidden.list", err)
	}
	if *idsOnly {
		ids := make([]string, len(result.Photos))
		for i, photo := range result.Photos {
			ids[i] = photo.PhotoID
		}
		writeJSON(stdout, ids)
		return 0
	}
	writeJSON(stdout, outputEnvelope{OK: true, Command: "photos.hidden.list", Result: result})
	return 0
}

func runSetDate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const command = "photos.set-date"
	flags := newFlagSet("set-date")
	datesFile := flags.String("dates-file", "", `JSON array of {"photoId": ID, "date": RFC3339}`)
	execute := flags.Bool("execute", false, "apply the creation-date change")
	dryRun := flags.Bool("dry-run", false, "preview the creation-date change")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return flagError(stdout, stderr, command, flags, err)
	}
	if *execute && *dryRun {
		return writeError(stderr, command, usageError("--execute and --dry-run cannot be used together"))
	}
	if *datesFile == "" {
		return writeError(stderr, command, usageError("--dates-file is required"))
	}
	raw, err := os.ReadFile(*datesFile)
	if err != nil {
		return writeError(stderr, command, &app.CodedError{Code: "input_error", Message: err.Error()})
	}
	var dates []photokit.PhotoDate
	if err := json.Unmarshal(raw, &dates); err != nil {
		return writeError(stderr, command, &app.CodedError{Code: "input_error", Message: "parse dates file: " + err.Error()})
	}
	result, err := photokit.SetCreationDates(ctx, dates, *execute)
	if err != nil {
		coded := photosError(err)
		if coded.Details == nil {
			coded.Details = make(map[string]any)
		}
		coded.Details["result"] = result
		return writeError(stderr, command, coded)
	}
	writeJSON(stdout, outputEnvelope{OK: true, Command: command, Result: result})
	return 0
}
