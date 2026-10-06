package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type memoryBackend struct {
	albums                   map[string]Album
	photos                   map[string]bool
	createCalls              int
	addCalls                 int
	importCalls              int
	importSkipDuplicateCheck bool
}

func (m *memoryBackend) ListAlbums(context.Context) ([]Album, error) {
	result := make([]Album, 0, len(m.albums))
	for _, album := range m.albums {
		result = append(result, Album{ID: album.ID, Name: album.Name})
	}
	return result, nil
}

func (m *memoryBackend) GetAlbum(_ context.Context, id string) (Album, error) {
	album, ok := m.albums[id]
	if !ok {
		return Album{}, errCode("not_found", "album was not found", map[string]any{"albumId": id})
	}
	count := len(album.PhotoIDs)
	album.PhotoCount = &count
	album.PhotoIDs = append([]string(nil), album.PhotoIDs...)
	return album, nil
}

func (m *memoryBackend) FindPhotoIDs(_ context.Context, ids []string) ([]string, error) {
	var found []string
	for _, id := range ids {
		if m.photos[id] {
			found = append(found, id)
		}
	}
	return found, nil
}

func (m *memoryBackend) CreateAlbum(_ context.Context, name string) (Album, error) {
	m.createCalls++
	album := Album{ID: "new-album", Name: name, PhotoIDs: []string{}}
	m.albums[album.ID] = album
	return album, nil
}

func (m *memoryBackend) AddPhotos(_ context.Context, albumID string, ids []string) error {
	m.addCalls++
	album := m.albums[albumID]
	album.PhotoIDs = append(album.PhotoIDs, ids...)
	m.albums[albumID] = album
	return nil
}

func (m *memoryBackend) ImportFiles(_ context.Context, albumID string, paths []string, skipDuplicateCheck bool) ([]string, error) {
	m.importCalls++
	m.importSkipDuplicateCheck = skipDuplicateCheck
	ids := make([]string, len(paths))
	album := m.albums[albumID]
	for i := range paths {
		ids[i] = fmt.Sprintf("imported-%d", i+1)
	}
	album.PhotoIDs = append(album.PhotoIDs, ids...)
	m.albums[albumID] = album
	return ids, nil
}

func (m *memoryBackend) Probe(context.Context) (int, error) { return len(m.albums), nil }

func newMemoryBackend() *memoryBackend {
	return &memoryBackend{
		albums: map[string]Album{"album-1": {ID: "album-1", Name: "Cars", PhotoIDs: []string{"photo-1"}}},
		photos: map[string]bool{"photo-1": true, "photo-2": true},
	}
}

func TestCreateIsDryRunAndIdempotent(t *testing.T) {
	ctx := context.Background()
	backend := newMemoryBackend()
	service := NewService(backend)

	dryRun, err := service.CreateAlbum(ctx, "New Cars", false)
	if err != nil {
		t.Fatal(err)
	}
	if !dryRun.DryRun || !dryRun.WouldCreate || backend.createCalls != 0 {
		t.Fatalf("unexpected dry-run result: %#v, calls=%d", dryRun, backend.createCalls)
	}

	created, err := service.CreateAlbum(ctx, "New Cars", true)
	if err != nil {
		t.Fatal(err)
	}
	if !created.Created || created.Album.ID != "new-album" || backend.createCalls != 1 {
		t.Fatalf("unexpected create result: %#v, calls=%d", created, backend.createCalls)
	}

	again, err := service.CreateAlbum(ctx, "New Cars", true)
	if err != nil {
		t.Fatal(err)
	}
	if again.Created || backend.createCalls != 1 {
		t.Fatalf("retry was not idempotent: %#v, calls=%d", again, backend.createCalls)
	}
}

func TestAddValidatesThenAddsOnlyMissingAndVerifies(t *testing.T) {
	ctx := context.Background()
	backend := newMemoryBackend()
	service := NewService(backend)
	service.sleep = func(time.Duration) {}

	result, err := service.AddPhotos(ctx, "album-1", []string{"photo-1", "photo-2", "photo-2"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.AlreadyPresent, []string{"photo-1"}) || !reflect.DeepEqual(result.Added, []string{"photo-2"}) {
		t.Fatalf("unexpected add result: %#v", result)
	}
	if backend.addCalls != 1 {
		t.Fatalf("expected one add, got %d", backend.addCalls)
	}

	retry, err := service.AddPhotos(ctx, "album-1", []string{"photo-1", "photo-2"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(retry.Added) != 0 || backend.addCalls != 1 {
		t.Fatalf("retry was not idempotent: %#v, calls=%d", retry, backend.addCalls)
	}
}

func TestAddMissingPhotoIsAtomic(t *testing.T) {
	backend := newMemoryBackend()
	service := NewService(backend)
	_, err := service.AddPhotos(context.Background(), "album-1", []string{"photo-2", "missing"}, true)
	if err == nil {
		t.Fatal("expected missing photo error")
	}
	var coded *CodedError
	if !errors.As(err, &coded) || coded.Code != "photos_not_found" {
		t.Fatalf("unexpected error: %v", err)
	}
	if backend.addCalls != 0 {
		t.Fatalf("add was called despite invalid input")
	}
}

func TestAddDryRunDoesNotMutate(t *testing.T) {
	backend := newMemoryBackend()
	service := NewService(backend)
	result, err := service.AddPhotos(context.Background(), "album-1", []string{"photo-2"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.DryRun || !reflect.DeepEqual(result.WouldAdd, []string{"photo-2"}) || backend.addCalls != 0 {
		t.Fatalf("unexpected dry-run result: %#v, calls=%d", result, backend.addCalls)
	}
}

func TestImportDryRunDoesNotMutate(t *testing.T) {
	backend := newMemoryBackend()
	service := NewService(backend)
	path := writeTempPhoto(t, "one.jpg")
	result, err := service.ImportFiles(context.Background(), "album-1", []string{path}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.DryRun || !result.SkipDuplicateCheck || !reflect.DeepEqual(result.WouldImportFiles, []string{path}) || backend.importCalls != 0 {
		t.Fatalf("unexpected dry-run result: %#v, calls=%d", result, backend.importCalls)
	}
	assertImportResultSkipDuplicateCheck(t, result, true)
}

func TestImportValidatesAllFilesBeforeMutationAndVerifies(t *testing.T) {
	backend := newMemoryBackend()
	service := NewService(backend)
	service.sleep = func(time.Duration) {}
	valid := writeTempPhoto(t, "one.jpg")
	missing := filepath.Join(t.TempDir(), "missing.jpg")

	_, err := service.ImportFiles(context.Background(), "album-1", []string{valid, missing}, false, true)
	var coded *CodedError
	if !errors.As(err, &coded) || coded.Code != "input_error" || backend.importCalls != 0 {
		t.Fatalf("expected atomic input error, got err=%v calls=%d", err, backend.importCalls)
	}

	result, err := service.ImportFiles(context.Background(), "album-1", []string{valid, valid}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.SkipDuplicateCheck || backend.importSkipDuplicateCheck || !reflect.DeepEqual(result.RequestedFiles, []string{valid}) || !reflect.DeepEqual(result.ImportedIDs, []string{"imported-1"}) || backend.importCalls != 1 {
		t.Fatalf("unexpected import result: %#v, calls=%d", result, backend.importCalls)
	}
	assertImportResultSkipDuplicateCheck(t, result, false)
}

func assertImportResultSkipDuplicateCheck(t *testing.T, result ImportResult, want bool) {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	if err := json.Unmarshal(data, &output); err != nil {
		t.Fatal(err)
	}
	if got, ok := output["skipDuplicateCheck"].(bool); !ok || got != want {
		t.Fatalf("skipDuplicateCheck=%#v, want %v", output["skipDuplicateCheck"], want)
	}
}

func writeTempPhoto(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("photo"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDuplicateAlbumNamesAreRejected(t *testing.T) {
	backend := newMemoryBackend()
	backend.albums["album-2"] = Album{ID: "album-2", Name: "Cars"}
	service := NewService(backend)

	for _, operation := range []func() error{
		func() error { _, err := service.GetAlbum(context.Background(), "", "Cars"); return err },
		func() error { _, err := service.CreateAlbum(context.Background(), "Cars", true); return err },
	} {
		err := operation()
		var coded *CodedError
		if !errors.As(err, &coded) || coded.Code != "ambiguous" {
			t.Fatalf("expected ambiguous error, got %v", err)
		}
	}
	if backend.createCalls != 0 {
		t.Fatalf("create was called despite ambiguity")
	}
}

func TestAddReportsFailedVerification(t *testing.T) {
	backend := newMemoryBackend()
	stale := &staleAddBackend{memoryBackend: backend}
	service := NewService(stale)
	service.sleep = func(time.Duration) {}
	_, err := service.AddPhotos(context.Background(), "album-1", []string{"photo-2"}, true)
	var coded *CodedError
	if !errors.As(err, &coded) || coded.Code != "verification_failed" {
		t.Fatalf("expected verification_failed, got %v", err)
	}
}

func TestImportReportsFailedVerification(t *testing.T) {
	backend := newMemoryBackend()
	stale := &staleImportBackend{memoryBackend: backend}
	service := NewService(stale)
	service.sleep = func(time.Duration) {}
	_, err := service.ImportFiles(context.Background(), "album-1", []string{writeTempPhoto(t, "one.jpg")}, false, true)
	var coded *CodedError
	if !errors.As(err, &coded) || coded.Code != "verification_failed" {
		t.Fatalf("expected verification_failed, got %v", err)
	}
}

type staleAddBackend struct{ *memoryBackend }

func (s *staleAddBackend) AddPhotos(context.Context, string, []string) error {
	s.addCalls++
	return nil
}

type staleImportBackend struct{ *memoryBackend }

func (s *staleImportBackend) ImportFiles(context.Context, string, []string, bool) ([]string, error) {
	s.importCalls++
	return []string{"not-in-album"}, nil
}

func TestMutationTimeoutStatesUnknownOutcome(t *testing.T) {
	for _, operation := range []string{"albums.add", "albums.import"} {
		err := timeoutError(operation)
		var coded *CodedError
		if !errors.As(err, &coded) || coded.Code != "timeout" || coded.Details["outcome"] != "unknown" {
			t.Fatalf("unexpected timeout error for %s: %#v", operation, err)
		}
	}
}
