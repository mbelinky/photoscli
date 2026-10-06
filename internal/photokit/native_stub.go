//go:build !darwin || !cgo

package photokit

import "context"

func nativeAuthStatus() (AuthorizationStatus, error) {
	return AuthorizationUnknown, unsupportedError()
}

func nativeRequestAuthorization(context.Context) (AuthorizationStatus, error) {
	return AuthorizationUnknown, unsupportedError()
}

func nativeRemoveFromAlbum(_ context.Context, albumID string, photoIDs []string, execute bool) (RemoveResult, error) {
	return RemoveResult{
		DryRun:            !execute,
		AlbumID:           albumID,
		RequestedPhotoIDs: append([]string(nil), photoIDs...),
	}, unsupportedError()
}

func nativeSetHidden(_ context.Context, photoIDs []string, hidden, execute bool) (SetHiddenResult, error) {
	return SetHiddenResult{DryRun: !execute, Hidden: hidden, RequestedPhotoIDs: append([]string(nil), photoIDs...)}, unsupportedError()
}

func nativeSetCreationDates(_ context.Context, dates []PhotoDate, execute bool) (SetDatesResult, error) {
	result := SetDatesResult{DryRun: !execute}
	for _, entry := range dates {
		result.RequestedPhotoIDs = append(result.RequestedPhotoIDs, entry.PhotoID)
	}
	return result, unsupportedError()
}

func unsupportedError() error {
	return &Error{Code: "unsupported", Message: "PhotoKit is available only on macOS builds with cgo enabled"}
}
