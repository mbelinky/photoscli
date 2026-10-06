package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ExportedImage is a rendered copy used for comparison. Width and Height are
// the original Photos dimensions, so a preview does not hide quality differences.
type ExportedImage struct {
	ID         string `json:"id"`
	Path       string `json:"path,omitempty"`
	Filename   string `json:"filename"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Favorite   bool   `json:"favorite"`
	Skipped    bool   `json:"skipped,omitempty"`
	SkipReason string `json:"skipReason,omitempty"`
}

// VideoSkipReason records the conservative classification used when the
// Photos scripting dictionary cannot provide a native media-type property.
const VideoSkipReason = "known_video_filename_extension"

type imageExportRequest struct {
	ID        string `json:"id"`
	Directory string `json:"directory"`
}

type ImageExporter interface {
	ExportImages(context.Context, []string, string) ([]ExportedImage, error)
}

// ExportImages exports each photo to a separate new directory, avoiding
// filename collisions and preserving the exact ID-to-file association.
func (b OsaBackend) ExportImages(ctx context.Context, ids []string, directory string) ([]ExportedImage, error) {
	requests := make([]imageExportRequest, 0, len(ids))
	for i, id := range ids {
		folder := filepath.Join(directory, fmt.Sprintf("photo-%06d", i))
		if err := os.Mkdir(folder, 0o700); err != nil {
			return nil, errCode("export_failed", "could not create a new photo export directory", map[string]any{"cause": err.Error()})
		}
		requests = append(requests, imageExportRequest{ID: id, Directory: folder})
	}
	var images []ExportedImage
	if err := b.call(ctx, "photos.export", map[string]any{"images": requests}, &images); err != nil {
		return nil, err
	}
	return validateExportedImages(ids, requests, images)
}

func validateExportedImages(ids []string, requests []imageExportRequest, images []ExportedImage) ([]ExportedImage, error) {
	if len(images) != len(ids) || len(requests) != len(ids) {
		return nil, errCode("export_failed", "Photos did not account for every requested media item", map[string]any{"requested": len(ids), "returned": len(images)})
	}
	for i := range images {
		if images[i].ID != ids[i] {
			return nil, errCode("export_failed", "Photos returned an unexpected media item ID", map[string]any{"expectedId": ids[i], "returnedId": images[i].ID})
		}
		entries, err := os.ReadDir(requests[i].Directory)
		if err != nil {
			return nil, errCode("export_failed", "could not inspect a media export directory", map[string]any{"photoId": images[i].ID, "cause": err.Error()})
		}
		if images[i].Skipped {
			if images[i].Path != "" || images[i].SkipReason != VideoSkipReason || !IsKnownVideoFilename(images[i].Filename) {
				return nil, errCode("export_failed", "Photos reported an unsupported media skip", map[string]any{"photoId": images[i].ID, "filename": images[i].Filename, "reason": images[i].SkipReason})
			}
			if len(entries) != 0 {
				return nil, errCode("export_failed", "a skipped video unexpectedly produced export files", map[string]any{"photoId": images[i].ID, "fileCount": len(entries)})
			}
			continue
		}
		if images[i].SkipReason != "" {
			return nil, errCode("export_failed", "Photos returned a skip reason for an unskipped media item", map[string]any{"photoId": images[i].ID, "reason": images[i].SkipReason})
		}
		if IsKnownVideoFilename(images[i].Filename) {
			return nil, errCode("export_failed", "a known video was not explicitly skipped", map[string]any{"photoId": images[i].ID, "filename": images[i].Filename})
		}
		var paths []string
		for _, entry := range entries {
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return nil, errCode("export_failed", "could not inspect an exported media file", map[string]any{"photoId": images[i].ID, "filename": entry.Name(), "cause": err.Error()})
			}
			if !info.Mode().IsRegular() {
				continue
			}
			if isSupportedImageFilename(entry.Name()) {
				paths = append(paths, filepath.Join(requests[i].Directory, entry.Name()))
			}
		}
		if len(paths) != 1 {
			return nil, errCode("export_failed", "expected exactly one supported rendered image for a photo", map[string]any{"photoId": images[i].ID, "imageFileCount": len(paths)})
		}
		images[i].Path = paths[0]
		if err := os.Chmod(paths[0], 0o600); err != nil {
			return nil, err
		}
	}
	return images, nil
}

// IsKnownVideoFilename is intentionally filename-based because Photos'
// documented scripting dictionary has no media-type property. Every match is
// surfaced to callers as a skip rather than silently omitted.
func IsKnownVideoFilename(filename string) bool {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".3g2", ".3gp", ".asf", ".avi", ".divx", ".dv", ".flv", ".hevc",
		".m2t", ".m2ts", ".m4v", ".mkv", ".mod", ".mov", ".mp4", ".mpeg",
		".mpg", ".mts", ".qt", ".tod", ".webm", ".wmv":
		return true
	default:
		return false
	}
}

func isSupportedImageFilename(filename string) bool {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".jpg", ".jpeg", ".png", ".heic", ".heif", ".tif", ".tiff", ".gif":
		return true
	default:
		return false
	}
}
