package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateExportedImagesAcceptsImageAndExplicitVideo(t *testing.T) {
	root := t.TempDir()
	imageDirectory := filepath.Join(root, "image")
	videoDirectory := filepath.Join(root, "video")
	for _, directory := range []string{imageDirectory, videoDirectory} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(imageDirectory, "rendered.JPG"), []byte("preview"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Photos may place Live Photo companions or metadata beside the rendered image.
	if err := os.WriteFile(filepath.Join(imageDirectory, "companion.mov"), []byte("sidecar"), 0o600); err != nil {
		t.Fatal(err)
	}
	media := []ExportedImage{
		{ID: "image", Filename: "IMG_0001.HEIC"},
		{ID: "video", Filename: "VID_0002.MOV", Skipped: true, SkipReason: VideoSkipReason},
	}
	validated, err := validateExportedImages(
		[]string{"image", "video"},
		[]imageExportRequest{{ID: "image", Directory: imageDirectory}, {ID: "video", Directory: videoDirectory}},
		media,
	)
	if err != nil {
		t.Fatal(err)
	}
	if validated[0].Path != filepath.Join(imageDirectory, "rendered.JPG") || validated[1].Path != "" || !validated[1].Skipped {
		t.Fatalf("unexpected validated exports: %+v", validated)
	}
}

func TestValidateExportedImagesRejectsMissingUnsupportedOrAmbiguousStill(t *testing.T) {
	for name, filenames := range map[string][]string{
		"missing":     nil,
		"unsupported": {"rendered.bmp"},
		"ambiguous":   {"first.jpg", "second.png"},
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			for _, filename := range filenames {
				if err := os.WriteFile(filepath.Join(directory, filename), []byte("unsupported"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := validateExportedImages(
				[]string{"still"},
				[]imageExportRequest{{ID: "still", Directory: directory}},
				[]ExportedImage{{ID: "still", Filename: "still.raw"}},
			)
			var coded *CodedError
			if !errors.As(err, &coded) || coded.Code != "export_failed" {
				t.Fatalf("expected visible export failure, got %v", err)
			}
		})
	}
}

func TestValidateExportedImagesRejectsIncompleteOrReorderedResults(t *testing.T) {
	tests := map[string]struct {
		ids      []string
		requests []imageExportRequest
		media    []ExportedImage
	}{
		"missing result": {
			ids:      []string{"one"},
			requests: []imageExportRequest{{ID: "one", Directory: t.TempDir()}},
		},
		"wrong ID": {
			ids:      []string{"one"},
			requests: []imageExportRequest{{ID: "one", Directory: t.TempDir()}},
			media:    []ExportedImage{{ID: "two"}},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := validateExportedImages(test.ids, test.requests, test.media); err == nil {
				t.Fatal("invalid export accounting was accepted")
			}
		})
	}
}

func TestValidateExportedImagesRejectsUnrecognizedSkip(t *testing.T) {
	for name, media := range map[string]ExportedImage{
		"still filename": {ID: "still", Filename: "still.jpg", Skipped: true, SkipReason: VideoSkipReason},
		"nonempty path":  {ID: "video", Path: "/tmp/clip.mov", Filename: "clip.mov", Skipped: true, SkipReason: VideoSkipReason},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := validateExportedImages(
				[]string{media.ID},
				[]imageExportRequest{{ID: media.ID, Directory: t.TempDir()}},
				[]ExportedImage{media},
			)
			if err == nil {
				t.Fatal("invalid video skip was accepted")
			}
		})
	}
}

func TestKnownVideoFilenameIsConservativeAndCaseInsensitive(t *testing.T) {
	for _, filename := range []string{"clip.MOV", "clip.mp4", "clip.M4V"} {
		if !IsKnownVideoFilename(filename) {
			t.Errorf("%q was not recognized as video", filename)
		}
	}
	for _, filename := range []string{"photo.jpg", "animation.gif", "unknown.raw", "no-extension"} {
		if IsKnownVideoFilename(filename) {
			t.Errorf("%q was incorrectly recognized as video", filename)
		}
	}
}
