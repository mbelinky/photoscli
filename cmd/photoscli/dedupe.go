package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/mbelinky/photoscli/internal/app"
	"github.com/mbelinky/photoscli/internal/duplicates"
	"github.com/mbelinky/photoscli/internal/photokit"
)

func runPhotosAuth(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("auth photos")
	request := flags.Bool("request", false, "show the standard macOS Photos permission prompt if needed")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return flagError(stdout, stderr, "auth.photos", flags, err)
	}
	var status photokit.AuthorizationStatus
	var err error
	if *request {
		status, err = photokit.RequestAuthorization(ctx)
	} else {
		status, err = photokit.AuthStatus()
	}
	if err != nil {
		return writeError(stderr, "auth.photos", photosError(err))
	}
	writeJSON(stdout, outputEnvelope{OK: true, Command: "auth.photos", Result: map[string]any{"photos": status, "requested": *request}})
	return 0
}

func runAlbumRemove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("albums remove")
	albumID := flags.String("album-id", "", "exact Photos album ID")
	var ids stringList
	flags.Var(&ids, "photo-id", "exact Photos media-item ID (repeatable)")
	idsFile := flags.String("photo-ids-file", "", "newline-delimited or JSON-array photo IDs")
	execute := flags.Bool("execute", false, "remove album membership while keeping library photos")
	dryRun := flags.Bool("dry-run", false, "preview removal without changing the album")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return flagError(stdout, stderr, "albums.remove", flags, err)
	}
	if *execute && *dryRun {
		return writeError(stderr, "albums.remove", usageError("--execute and --dry-run cannot be used together"))
	}
	if *idsFile != "" {
		fileIDs, err := readIDs(*idsFile)
		if err != nil {
			return writeError(stderr, "albums.remove", &app.CodedError{Code: "input_error", Message: err.Error()})
		}
		ids = append(ids, fileIDs...)
	}
	result, err := photokit.RemoveFromAlbum(ctx, *albumID, ids, *execute)
	if err != nil {
		coded := photosError(err)
		if coded.Details == nil {
			coded.Details = make(map[string]any)
		}
		coded.Details["result"] = result
		return writeError(stderr, "albums.remove", coded)
	}
	writeJSON(stdout, outputEnvelope{OK: true, Command: "albums.remove", Result: result})
	return 0
}

func photosError(err error) *app.CodedError {
	var native *photokit.Error
	if errors.As(err, &native) {
		code := native.Code
		if code == "invalid_input" {
			code = "input_error"
		}
		details := make(map[string]any, len(native.Details)+1)
		for key, value := range native.Details {
			details[key] = value
		}
		details["readbackRequired"] = native.ReadbackRequired
		return &app.CodedError{Code: code, Message: native.Message, Details: details}
	}
	return &app.CodedError{Code: "photokit_error", Message: err.Error()}
}

func runAlbumDuplicates(ctx context.Context, args []string, stdout, stderr io.Writer, service *app.Service, backend app.Backend) int {
	flags := newFlagSet("albums duplicates")
	albumID := flags.String("album-id", "", "exact Photos album ID")
	imagesFile := flags.String("images-file", "", "JSON array of photo IDs, exported image paths, and optional original dimensions")
	outputDir := flags.String("output-dir", "", "parent directory for a new private export directory")
	distance := flags.Int("distance", 12, "maximum perceptual difference; candidates still require visual review")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return flagError(stdout, stderr, "albums.duplicates", flags, err)
	}
	if *albumID == "" || (*imagesFile != "" && *outputDir != "") || *distance < 1 || *distance > 256 {
		return writeError(stderr, "albums.duplicates", usageError("provide --album-id, choose either --images-file or --output-dir, and use --distance between 1 and 256"))
	}
	album, err := service.GetAlbum(ctx, *albumID, "")
	if err != nil {
		return writeError(stderr, "albums.duplicates", err)
	}
	var images []duplicates.ImageInput
	var skippedMedia []app.ExportedImage
	var scanDir string
	if *imagesFile != "" {
		data, err := os.ReadFile(*imagesFile)
		if err != nil {
			return writeError(stderr, "albums.duplicates", &app.CodedError{Code: "input_error", Message: err.Error()})
		}
		var manifest []app.ExportedImage
		if err := json.Unmarshal(data, &manifest); err != nil {
			return writeError(stderr, "albums.duplicates", &app.CodedError{Code: "input_error", Message: err.Error()})
		}
		manifestPath, err := filepath.Abs(*imagesFile)
		if err != nil {
			return writeError(stderr, "albums.duplicates", err)
		}
		for i := range manifest {
			if manifest[i].Path != "" && !filepath.IsAbs(manifest[i].Path) {
				manifest[i].Path = filepath.Join(filepath.Dir(manifestPath), manifest[i].Path)
			}
		}
		images, skippedMedia, err = albumImages(album.PhotoIDs, manifest)
		if err != nil {
			return writeError(stderr, "albums.duplicates", err)
		}
	} else if len(album.PhotoIDs) != 0 {
		exporter, ok := backend.(app.ImageExporter)
		if !ok {
			return writeError(stderr, "albums.duplicates", &app.CodedError{Code: "unsupported", Message: "the Photos backend does not support image exports"})
		}
		parent := *outputDir
		if parent == "" {
			cache, err := os.UserCacheDir()
			if err != nil {
				return writeError(stderr, "albums.duplicates", err)
			}
			parent = filepath.Join(cache, "photoscli", "duplicates")
		}
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return writeError(stderr, "albums.duplicates", err)
		}
		scanDir, err = os.MkdirTemp(parent, "scan-")
		if err != nil {
			return writeError(stderr, "albums.duplicates", err)
		}
		exported, err := exporter.ExportImages(ctx, album.PhotoIDs, scanDir)
		if err != nil {
			return writeError(stderr, "albums.duplicates", err)
		}
		images, skippedMedia, err = albumImages(album.PhotoIDs, exported)
		if err != nil {
			return writeError(stderr, "albums.duplicates", err)
		}
		data, err := json.MarshalIndent(exported, "", "  ")
		if err != nil {
			return writeError(stderr, "albums.duplicates", err)
		}
		if err := os.WriteFile(filepath.Join(scanDir, "images.json"), data, 0o600); err != nil {
			return writeError(stderr, "albums.duplicates", err)
		}
	}
	report, scanErr := duplicates.Scan(ctx, images, duplicates.Options{DHashThreshold: *distance})
	result := map[string]any{
		"album":           album,
		"report":          report,
		"exportDirectory": scanDir,
		"skippedMedia":    skippedMedia,
		"mediaCounts": map[string]int{
			"albumMembers":  len(album.PhotoIDs),
			"imagesScanned": report.Counts.Scanned,
			"videosSkipped": len(skippedMedia),
		},
		"skipPolicy":    "Photos Automation exposes no media-type property; items with a known movie filename extension are reported and excluded from image comparison.",
		"removalPolicy": "Only confirmed copies may be removed from album membership. No library photos are deleted.",
	}
	if scanErr != nil {
		writeJSON(stderr, outputEnvelope{OK: false, Command: "albums.duplicates", Result: result, Error: &app.CodedError{Code: "scan_incomplete", Message: scanErr.Error()}})
		return 1
	}
	writeJSON(stdout, outputEnvelope{OK: true, Command: "albums.duplicates", Result: result})
	return 0
}

// albumImages requires complete, unambiguous coverage of the current album.
// Known videos remain explicit manifest entries and are returned separately;
// every other entry remains required input to the image scanner.
func albumImages(ids []string, media []app.ExportedImage) ([]duplicates.ImageInput, []app.ExportedImage, error) {
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	byID := make(map[string]app.ExportedImage, len(ids))
	for _, item := range media {
		if !wanted[item.ID] {
			continue
		}
		if prior, exists := byID[item.ID]; exists && prior != item {
			return nil, nil, &app.CodedError{Code: "input_error", Message: "conflicting manifest entries for one album media item", Details: map[string]any{"photoId": item.ID}}
		}
		byID[item.ID] = item
	}
	selected := make([]duplicates.ImageInput, 0, len(ids))
	skipped := make([]app.ExportedImage, 0)
	for _, id := range ids {
		item, exists := byID[id]
		if !exists {
			return nil, nil, &app.CodedError{Code: "input_error", Message: "the image manifest does not cover every current album media item", Details: map[string]any{"missingPhotoId": id}}
		}
		if item.Skipped {
			if item.Path != "" || item.SkipReason != app.VideoSkipReason || !app.IsKnownVideoFilename(item.Filename) {
				return nil, nil, &app.CodedError{Code: "input_error", Message: "a skipped manifest entry is not a recognized video", Details: map[string]any{"photoId": id, "filename": item.Filename, "reason": item.SkipReason}}
			}
			skipped = append(skipped, item)
			continue
		}
		if item.SkipReason != "" {
			return nil, nil, &app.CodedError{Code: "input_error", Message: "an unskipped manifest entry has a skip reason", Details: map[string]any{"photoId": id, "reason": item.SkipReason}}
		}
		if app.IsKnownVideoFilename(item.Filename) {
			return nil, nil, &app.CodedError{Code: "input_error", Message: "a known video must be explicitly marked as skipped", Details: map[string]any{"photoId": id, "filename": item.Filename}}
		}
		selected = append(selected, duplicates.ImageInput{ID: item.ID, Path: item.Path, Filename: item.Filename, Width: item.Width, Height: item.Height, Favorite: item.Favorite})
	}
	return selected, skipped, nil
}
