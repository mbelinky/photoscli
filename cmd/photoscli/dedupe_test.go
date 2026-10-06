package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbelinky/photoscli/internal/app"
	"github.com/mbelinky/photoscli/internal/duplicates"
)

func TestAlbumImagesRejectsMissingAndConflictingCoverage(t *testing.T) {
	media := []app.ExportedImage{{ID: "a", Path: "/a.jpg"}, {ID: "unrelated", Path: "/other.jpg"}}
	if _, _, err := albumImages([]string{"a", "b"}, media); err == nil {
		t.Fatal("partial album coverage must not be presented as a complete scan")
	}
	media = append(media, app.ExportedImage{ID: "a", Path: "/different.jpg"})
	if _, _, err := albumImages([]string{"a"}, media); err == nil {
		t.Fatal("one ID cannot be associated with conflicting image files")
	}
}

func TestAlbumImagesScopesAndOrdersCurrentMembership(t *testing.T) {
	media := []app.ExportedImage{{ID: "b", Path: "/b.jpg"}, {ID: "outside", Path: "/outside.jpg"}, {ID: "a", Path: "/a.jpg"}}
	selected, skipped, err := albumImages([]string{"a", "b"}, media)
	if err != nil || len(skipped) != 0 || len(selected) != 2 || selected[0].ID != "a" || selected[1].ID != "b" {
		t.Fatalf("unexpected scoped scan: selected=%+v skipped=%+v err=%v", selected, skipped, err)
	}
}

func TestDuplicateScanDefaultExportReportsMixedAlbumVideo(t *testing.T) {
	photoPath := writeTestPNG(t, "photo.png")
	backend := &duplicateTestBackend{
		album: app.Album{ID: "album", Name: "Mixed", PhotoIDs: []string{"photo", "video"}},
		exported: []app.ExportedImage{
			{ID: "photo", Path: photoPath, Filename: "IMG_0001.HEIC", Width: 100, Height: 80},
			{ID: "video", Filename: "VID_0002.MOV", Width: 1920, Height: 1080, Skipped: true, SkipReason: app.VideoSkipReason},
		},
	}
	stdout, stderr, code := runDuplicateCommand(t, backend,
		"albums", "duplicates", "--album-id", "album", "--output-dir", t.TempDir())
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	result := decodeDuplicateResult(t, stdout)
	if result.Report.Counts.Scanned != 1 || result.MediaCounts["videosSkipped"] != 1 ||
		len(result.SkippedMedia) != 1 || result.SkippedMedia[0].ID != "video" ||
		result.SkippedMedia[0].SkipReason != app.VideoSkipReason {
		t.Fatalf("unexpected mixed result: %+v", result)
	}
	manifestData, err := os.ReadFile(filepath.Join(result.ExportDirectory, "images.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest []app.ExportedImage
	if err := json.Unmarshal(manifestData, &manifest); err != nil || len(manifest) != 2 || !manifest[1].Skipped {
		t.Fatalf("generated manifest=%+v err=%v", manifest, err)
	}
}

func TestDuplicateScanManifestReportsMixedAlbumVideo(t *testing.T) {
	photoPath := writeTestPNG(t, "photo.png")
	manifestPath := writeDuplicateManifest(t, []app.ExportedImage{
		{ID: "photo", Path: photoPath, Filename: "photo.jpg"},
		{ID: "video", Filename: "clip.mp4", Skipped: true, SkipReason: app.VideoSkipReason},
	})
	backend := &duplicateTestBackend{album: app.Album{ID: "album", PhotoIDs: []string{"photo", "video"}}}
	stdout, stderr, code := runDuplicateCommand(t, backend,
		"albums", "duplicates", "--album-id", "album", "--images-file", manifestPath)
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	result := decodeDuplicateResult(t, stdout)
	if result.Report.Counts.Scanned != 1 || len(result.SkippedMedia) != 1 || result.SkippedMedia[0].Filename != "clip.mp4" {
		t.Fatalf("unexpected manifest result: %+v", result)
	}
}

func TestDuplicateScanAllVideoAlbumSucceedsWithExplicitSkips(t *testing.T) {
	manifestPath := writeDuplicateManifest(t, []app.ExportedImage{
		{ID: "video-a", Filename: "one.MOV", Skipped: true, SkipReason: app.VideoSkipReason},
		{ID: "video-b", Filename: "two.MP4", Skipped: true, SkipReason: app.VideoSkipReason},
	})
	backend := &duplicateTestBackend{album: app.Album{ID: "album", PhotoIDs: []string{"video-a", "video-b"}}}
	stdout, stderr, code := runDuplicateCommand(t, backend,
		"albums", "duplicates", "--album-id", "album", "--images-file", manifestPath)
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	result := decodeDuplicateResult(t, stdout)
	if result.Report.Counts.Requested != 0 || result.Report.Counts.Scanned != 0 ||
		len(result.SkippedMedia) != 2 || result.MediaCounts["videosSkipped"] != 2 {
		t.Fatalf("unexpected all-video result: %+v", result)
	}
}

func TestDuplicateManifestCannotOmitStillImageCoverage(t *testing.T) {
	manifestPath := writeDuplicateManifest(t, []app.ExportedImage{
		{ID: "video", Filename: "clip.mov", Skipped: true, SkipReason: app.VideoSkipReason},
	})
	backend := &duplicateTestBackend{album: app.Album{ID: "album", PhotoIDs: []string{"still", "video"}}}
	_, stderr, code := runDuplicateCommand(t, backend,
		"albums", "duplicates", "--album-id", "album", "--images-file", manifestPath)
	if code != 2 || !bytes.Contains([]byte(stderr), []byte("does not cover every current album media item")) {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

func TestDuplicateManifestUnsupportedStillImageFailsVisibly(t *testing.T) {
	unsupported := filepath.Join(t.TempDir(), "still.tiff")
	if err := os.WriteFile(unsupported, []byte("not a decodable image"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestPath := writeDuplicateManifest(t, []app.ExportedImage{{ID: "still", Path: unsupported, Filename: "still.tiff"}})
	backend := &duplicateTestBackend{album: app.Album{ID: "album", PhotoIDs: []string{"still"}}}
	_, stderr, code := runDuplicateCommand(t, backend,
		"albums", "duplicates", "--album-id", "album", "--images-file", manifestPath)
	if code != 1 || !bytes.Contains([]byte(stderr), []byte("scan_incomplete")) ||
		!bytes.Contains([]byte(stderr), []byte("image: unknown format")) {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

func TestManifestCannotClaimStillImageWasSkippedAsVideo(t *testing.T) {
	_, _, err := albumImages([]string{"still"}, []app.ExportedImage{
		{ID: "still", Filename: "still.jpg", Skipped: true, SkipReason: app.VideoSkipReason},
	})
	if err == nil {
		t.Fatal("a still-image filename must not be accepted as a video skip")
	}
}

func TestManifestSkippedVideoCannotHaveAPath(t *testing.T) {
	_, _, err := albumImages([]string{"video"}, []app.ExportedImage{
		{ID: "video", Path: "/tmp/clip.mov", Filename: "clip.mov", Skipped: true, SkipReason: app.VideoSkipReason},
	})
	if err == nil {
		t.Fatal("a skipped video with an export path must be rejected")
	}
}

func TestNewCommandHelpAndInvalidRemovalAvoidPhotos(t *testing.T) {
	for _, args := range [][]string{{"albums", "duplicates", "--help"}, {"albums", "remove", "--help"}, {"auth", "photos", "--help"}, {"photos", "hide", "--help"}, {"photos", "unhide", "--help"}, {"photos", "hidden", "list", "--help"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr, nil); code != 0 || stderr.Len() != 0 {
			t.Fatalf("help failed: %q: %d, %s", args, code, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"albums", "remove", "--album-id", "a", "--photo-id", "p", "--execute", "--dry-run"}, &stdout, &stderr, nil); code != 2 {
		t.Fatalf("conflicting write flags must be rejected before native access: %d", code)
	}
}

func TestPhotoCurationUsageRejectsInvalidInputBeforeNativeAccess(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"photos", "hide", "--photo-id", "p"}, &stdout, &stderr, nil); code != 2 {
		t.Fatalf("missing reason must be rejected before native access: %d", code)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"photos", "unhide", "--photo-id", "p", "--reason", "review", "--execute", "--dry-run"}, &stdout, &stderr, nil); code != 2 {
		t.Fatalf("conflicting write flags must be rejected before native access: %d", code)
	}
}

type duplicateTestBackend struct {
	album    app.Album
	exported []app.ExportedImage
}

func (backend *duplicateTestBackend) ListAlbums(context.Context) ([]app.Album, error) {
	return []app.Album{backend.album}, nil
}
func (backend *duplicateTestBackend) GetAlbum(context.Context, string) (app.Album, error) {
	album := backend.album
	album.PhotoIDs = append([]string(nil), album.PhotoIDs...)
	return album, nil
}
func (*duplicateTestBackend) FindPhotoIDs(context.Context, []string) ([]string, error) {
	return nil, nil
}
func (*duplicateTestBackend) CreateAlbum(context.Context, string) (app.Album, error) {
	return app.Album{}, nil
}
func (*duplicateTestBackend) AddPhotos(context.Context, string, []string) error { return nil }
func (*duplicateTestBackend) ImportFiles(context.Context, string, []string, bool) ([]string, error) {
	return nil, nil
}
func (*duplicateTestBackend) Probe(context.Context) (int, error) { return 0, nil }
func (backend *duplicateTestBackend) ExportImages(context.Context, []string, string) ([]app.ExportedImage, error) {
	return append([]app.ExportedImage(nil), backend.exported...), nil
}

func runDuplicateCommand(t *testing.T, backend app.Backend, args ...string) (stdout string, stderr string, code int) {
	t.Helper()
	var out, err bytes.Buffer
	code = run(args, &out, &err, backend)
	return out.String(), err.String(), code
}

type duplicateCommandResult struct {
	Report          duplicates.Report   `json:"report"`
	ExportDirectory string              `json:"exportDirectory"`
	SkippedMedia    []app.ExportedImage `json:"skippedMedia"`
	MediaCounts     map[string]int      `json:"mediaCounts"`
}

func decodeDuplicateResult(t *testing.T, output string) duplicateCommandResult {
	t.Helper()
	var envelope struct {
		OK     bool                   `json:"ok"`
		Result duplicateCommandResult `json:"result"`
	}
	if err := json.Unmarshal([]byte(output), &envelope); err != nil {
		t.Fatalf("decode duplicate output: %v; output=%s", err, output)
	}
	if !envelope.OK {
		t.Fatalf("duplicate command failed: %s", output)
	}
	return envelope.Result
}

func writeDuplicateManifest(t *testing.T, media []app.ExportedImage) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "images.json")
	data, err := json.Marshal(media)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTestPNG(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.NRGBA{R: 255, A: 255})
	if err := png.Encode(file, img); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
