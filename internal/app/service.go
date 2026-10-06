package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type Service struct {
	backend Backend
	sleep   func(time.Duration)
}

func NewService(backend Backend) *Service {
	return &Service{backend: backend, sleep: time.Sleep}
}

func (s *Service) ListAlbums(ctx context.Context, name string) ([]Album, error) {
	albums, err := s.backend.ListAlbums(ctx)
	if err != nil {
		return nil, err
	}
	filtered := albums[:0]
	for _, album := range albums {
		if name == "" || album.Name == name {
			filtered = append(filtered, album)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Name == filtered[j].Name {
			return filtered[i].ID < filtered[j].ID
		}
		return filtered[i].Name < filtered[j].Name
	})
	return filtered, nil
}

func (s *Service) GetAlbum(ctx context.Context, id, name string) (Album, error) {
	if (id == "") == (name == "") {
		return Album{}, errCode("usage", "provide exactly one of --id or --name", nil)
	}
	if id != "" {
		return s.backend.GetAlbum(ctx, id)
	}
	albums, err := s.ListAlbums(ctx, name)
	if err != nil {
		return Album{}, err
	}
	if len(albums) == 0 {
		return Album{}, errCode("not_found", fmt.Sprintf("album %q was not found", name), nil)
	}
	if len(albums) > 1 {
		ids := make([]string, 0, len(albums))
		for _, album := range albums {
			ids = append(ids, album.ID)
		}
		return Album{}, errCode("ambiguous", fmt.Sprintf("more than one album is named %q; use --id", name), map[string]any{"albumIds": ids})
	}
	return s.backend.GetAlbum(ctx, albums[0].ID)
}

type CreateResult struct {
	DryRun      bool  `json:"dryRun"`
	Created     bool  `json:"created"`
	WouldCreate bool  `json:"wouldCreate"`
	Album       Album `json:"album"`
}

func (s *Service) CreateAlbum(ctx context.Context, name string, execute bool) (CreateResult, error) {
	if name == "" {
		return CreateResult{}, errCode("usage", "--name is required", nil)
	}
	matches, err := s.ListAlbums(ctx, name)
	if err != nil {
		return CreateResult{}, err
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, album := range matches {
			ids = append(ids, album.ID)
		}
		return CreateResult{}, errCode("ambiguous", fmt.Sprintf("more than one album is named %q", name), map[string]any{"albumIds": ids})
	}
	if len(matches) == 1 {
		album, err := s.backend.GetAlbum(ctx, matches[0].ID)
		if err != nil {
			return CreateResult{}, err
		}
		return CreateResult{DryRun: !execute, Album: album}, nil
	}
	if !execute {
		return CreateResult{DryRun: true, WouldCreate: true, Album: Album{Name: name}}, nil
	}
	album, err := s.backend.CreateAlbum(ctx, name)
	if err != nil {
		return CreateResult{}, err
	}
	verified, err := s.backend.GetAlbum(ctx, album.ID)
	if err != nil {
		return CreateResult{}, errCode("verification_failed", "album was created but could not be read back", map[string]any{"albumId": album.ID, "cause": err.Error()})
	}
	if verified.Name != name {
		return CreateResult{}, errCode("verification_failed", "created album name did not match", map[string]any{"albumId": album.ID, "actualName": verified.Name})
	}
	return CreateResult{Created: true, Album: verified}, nil
}

type AddResult struct {
	DryRun         bool     `json:"dryRun"`
	Album          Album    `json:"album"`
	RequestedIDs   []string `json:"requestedPhotoIds"`
	AlreadyPresent []string `json:"alreadyPresentPhotoIds"`
	Added          []string `json:"addedPhotoIds"`
	WouldAdd       []string `json:"wouldAddPhotoIds"`
}

func (s *Service) AddPhotos(ctx context.Context, albumID string, requested []string, execute bool) (AddResult, error) {
	if albumID == "" {
		return AddResult{}, errCode("usage", "--album-id is required", nil)
	}
	ids := uniqueNonempty(requested)
	if len(ids) == 0 {
		return AddResult{}, errCode("usage", "at least one --photo-id or --photo-ids-file entry is required", nil)
	}
	album, err := s.backend.GetAlbum(ctx, albumID)
	if err != nil {
		return AddResult{}, err
	}
	found, err := s.backend.FindPhotoIDs(ctx, ids)
	if err != nil {
		return AddResult{}, err
	}
	foundSet := toSet(found)
	missing := difference(ids, foundSet)
	if len(missing) > 0 {
		return AddResult{}, errCode("photos_not_found", "one or more photo IDs were not found; nothing was added", map[string]any{"photoIds": missing})
	}
	presentSet := toSet(album.PhotoIDs)
	already := intersection(ids, presentSet)
	toAdd := difference(ids, presentSet)
	result := AddResult{DryRun: !execute, Album: album, RequestedIDs: ids, AlreadyPresent: already}
	if !execute {
		result.WouldAdd = toAdd
		return result, nil
	}
	if len(toAdd) == 0 {
		return result, nil
	}
	if err := s.backend.AddPhotos(ctx, albumID, toAdd); err != nil {
		return AddResult{}, err
	}
	var latest Album
	for attempt := 0; attempt < 10; attempt++ {
		latest, err = s.backend.GetAlbum(ctx, albumID)
		if err == nil && len(difference(toAdd, toSet(latest.PhotoIDs))) == 0 {
			result.Album = latest
			result.Added = toAdd
			return result, nil
		}
		if attempt < 9 {
			s.sleep(200 * time.Millisecond)
		}
	}
	unverified := toAdd
	if err == nil {
		unverified = difference(toAdd, toSet(latest.PhotoIDs))
	}
	details := map[string]any{"albumId": albumID, "photoIds": unverified}
	if err != nil {
		details["cause"] = err.Error()
	}
	return AddResult{}, errCode("verification_failed", "Photos accepted the add request but membership could not be verified", details)
}

type ImportResult struct {
	DryRun             bool     `json:"dryRun"`
	SkipDuplicateCheck bool     `json:"skipDuplicateCheck"`
	Album              Album    `json:"album"`
	RequestedFiles     []string `json:"requestedFiles"`
	ImportedIDs        []string `json:"importedPhotoIds"`
	WouldImportFiles   []string `json:"wouldImportFiles"`
}

func (s *Service) ImportFiles(ctx context.Context, albumID string, requested []string, skipDuplicateCheck, execute bool) (ImportResult, error) {
	if albumID == "" {
		return ImportResult{}, errCode("usage", "--album-id is required", nil)
	}
	paths, err := regularFilePaths(requested)
	if err != nil {
		return ImportResult{}, err
	}
	if len(paths) == 0 {
		return ImportResult{}, errCode("usage", "at least one --file or --files-file entry is required", nil)
	}
	album, err := s.backend.GetAlbum(ctx, albumID)
	if err != nil {
		return ImportResult{}, err
	}
	result := ImportResult{DryRun: !execute, SkipDuplicateCheck: skipDuplicateCheck, Album: album, RequestedFiles: paths}
	if !execute {
		result.WouldImportFiles = paths
		return result, nil
	}
	imported, err := s.backend.ImportFiles(ctx, albumID, paths, skipDuplicateCheck)
	if err != nil {
		return ImportResult{}, err
	}
	imported = uniqueNonempty(imported)
	var latest Album
	for attempt := 0; attempt < 10; attempt++ {
		latest, err = s.backend.GetAlbum(ctx, albumID)
		if err == nil && len(difference(imported, toSet(latest.PhotoIDs))) == 0 {
			result.Album = latest
			result.ImportedIDs = imported
			return result, nil
		}
		if attempt < 9 {
			s.sleep(200 * time.Millisecond)
		}
	}
	unverified := imported
	if err == nil {
		unverified = difference(imported, toSet(latest.PhotoIDs))
	}
	details := map[string]any{"albumId": albumID, "photoIds": unverified}
	if err != nil {
		details["cause"] = err.Error()
	}
	return ImportResult{}, errCode("verification_failed", "Photos accepted the import request but album membership could not be verified", details)
}

func regularFilePaths(requested []string) ([]string, error) {
	seen := map[string]bool{}
	paths := make([]string, 0, len(requested))
	for _, path := range requested {
		if path == "" {
			return nil, errCode("input_error", "file path must not be empty", nil)
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, errCode("input_error", "could not resolve file path", map[string]any{"path": path, "cause": err.Error()})
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return nil, errCode("input_error", "could not read import file", map[string]any{"path": absolute, "cause": err.Error()})
		}
		if !info.Mode().IsRegular() {
			return nil, errCode("input_error", "import path is not a regular file", map[string]any{"path": absolute})
		}
		if !seen[absolute] {
			seen[absolute] = true
			paths = append(paths, absolute)
		}
	}
	return paths, nil
}

func uniqueNonempty(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func toSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func difference(values []string, set map[string]bool) []string {
	var result []string
	for _, value := range values {
		if !set[value] {
			result = append(result, value)
		}
	}
	return result
}

func intersection(values []string, set map[string]bool) []string {
	var result []string
	for _, value := range values {
		if set[value] {
			result = append(result, value)
		}
	}
	return result
}
