// Package photokit provides the narrow native Photos operations that cannot be
// expressed safely through AppleScript. Removing an asset only changes its
// membership in one user album; it never deletes the asset from the library.
package photokit

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// AuthorizationStatus is the current read/write Photos authorization state.
type AuthorizationStatus string

const (
	AuthorizationNotDetermined AuthorizationStatus = "not_determined"
	AuthorizationRestricted    AuthorizationStatus = "restricted"
	AuthorizationDenied        AuthorizationStatus = "denied"
	AuthorizationAuthorized    AuthorizationStatus = "authorized"
	AuthorizationLimited       AuthorizationStatus = "limited"
	AuthorizationUnknown       AuthorizationStatus = "unknown"
)

// Error is a stable, machine-readable failure returned by this package.
type Error struct {
	Code             string         `json:"code"`
	Message          string         `json:"message"`
	ReadbackRequired bool           `json:"readbackRequired,omitempty"`
	Details          map[string]any `json:"details,omitempty"`
	Cause            error          `json:"-"`
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Cause }

// RemoveResult describes the validated plan or the verified mutation.
type RemoveResult struct {
	DryRun                 bool     `json:"dryRun"`
	Executed               bool     `json:"executed"`
	Verified               bool     `json:"verified"`
	OutcomeUnknown         bool     `json:"outcomeUnknown,omitempty"`
	AlbumID                string   `json:"albumId"`
	AlbumTitle             string   `json:"albumTitle,omitempty"`
	RequestedPhotoIDs      []string `json:"requestedPhotoIds"`
	WouldRemovePhotoIDs    []string `json:"wouldRemovePhotoIds,omitempty"`
	RemovedPhotoIDs        []string `json:"removedPhotoIds,omitempty"`
	MissingPhotoIDs        []string `json:"missingPhotoIds,omitempty"`
	NotAlbumMemberPhotoIDs []string `json:"notAlbumMembersPhotoIds,omitempty"`
	StillInAlbumPhotoIDs   []string `json:"stillInAlbumPhotoIds,omitempty"`
	MissingPriorMemberIDs  []string `json:"missingPriorMemberPhotoIds,omitempty"`
	MissingFromLibraryIDs  []string `json:"missingFromLibraryPhotoIds,omitempty"`
	BeforeMemberCount      int      `json:"beforeMemberCount"`
	AfterMemberCount       int      `json:"afterMemberCount,omitempty"`
}

// SetHiddenResult describes the validated plan or verified hidden-state change.
type SetHiddenResult struct {
	DryRun                     bool     `json:"dryRun"`
	Executed                   bool     `json:"executed"`
	Verified                   bool     `json:"verified"`
	OutcomeUnknown             bool     `json:"outcomeUnknown,omitempty"`
	Hidden                     bool     `json:"hidden"`
	RequestedPhotoIDs          []string `json:"requestedPhotoIds"`
	WouldChangePhotoIDs        []string `json:"wouldChangePhotoIds,omitempty"`
	AlreadyInStatePhotoIDs     []string `json:"alreadyInStatePhotoIds,omitempty"`
	ChangedPhotoIDs            []string `json:"changedPhotoIds,omitempty"`
	MissingPhotoIDs            []string `json:"missingPhotoIds,omitempty"`
	NotInStatePhotoIDs         []string `json:"notInStatePhotoIds,omitempty"`
	MissingFromLibraryPhotoIDs []string `json:"missingFromLibraryPhotoIds,omitempty"`
}

// SetDatesResult describes the validated plan or verified creation-date change.
type SetDatesResult struct {
	DryRun                     bool     `json:"dryRun"`
	Executed                   bool     `json:"executed"`
	Verified                   bool     `json:"verified"`
	OutcomeUnknown             bool     `json:"outcomeUnknown,omitempty"`
	RequestedPhotoIDs          []string `json:"requestedPhotoIds"`
	WouldChangePhotoIDs        []string `json:"wouldChangePhotoIds,omitempty"`
	AlreadyInStatePhotoIDs     []string `json:"alreadyInStatePhotoIds,omitempty"`
	ChangedPhotoIDs            []string `json:"changedPhotoIds,omitempty"`
	MissingPhotoIDs            []string `json:"missingPhotoIds,omitempty"`
	NotInStatePhotoIDs         []string `json:"notInStatePhotoIds,omitempty"`
	MissingFromLibraryPhotoIDs []string `json:"missingFromLibraryPhotoIds,omitempty"`
}

// PhotoDate pairs one exact Photos local identifier with its wanted date.
type PhotoDate struct {
	PhotoID string    `json:"photoId"`
	Date    time.Time `json:"date"`
}

// AuthStatus reads the current Photos read/write authorization status. It never
// requests authorization and therefore never displays a system prompt.
func AuthStatus() (AuthorizationStatus, error) {
	return nativeAuthStatus()
}

// RequestAuthorization explicitly requests Photos read/write authorization.
// This is the only function in this package that can display the system prompt.
func RequestAuthorization(ctx context.Context) (AuthorizationStatus, error) {
	if ctx == nil {
		return AuthorizationUnknown, inputError("context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return AuthorizationUnknown, contextError(err, false)
	}
	return nativeRequestAuthorization(ctx)
}

// RemoveFromAlbum removes the exact photo identifiers from the exact user
// album identifier. With execute=false it validates the complete request and
// returns the change that would be made without mutating Photos.
func RemoveFromAlbum(ctx context.Context, albumID string, photoIDs []string, execute bool) (RemoveResult, error) {
	result := RemoveResult{
		DryRun:            !execute,
		AlbumID:           albumID,
		RequestedPhotoIDs: append([]string(nil), photoIDs...),
	}
	if ctx == nil {
		return result, inputError("context must not be nil")
	}
	if albumID == "" {
		return result, inputError("album ID must not be empty")
	}
	ids, err := validatePhotoIDs(photoIDs)
	if err != nil {
		return result, err
	}
	result.RequestedPhotoIDs = ids
	if err := ctx.Err(); err != nil {
		return result, contextError(err, false)
	}
	return nativeRemoveFromAlbum(ctx, albumID, ids, execute)
}

// SetHidden sets the hidden state for exact Photos local identifiers. With
// execute=false it validates the complete request and returns the planned
// change without mutating Photos.
func SetHidden(ctx context.Context, photoIDs []string, hidden, execute bool) (SetHiddenResult, error) {
	result := SetHiddenResult{DryRun: !execute, Hidden: hidden, RequestedPhotoIDs: append([]string(nil), photoIDs...)}
	if ctx == nil {
		return result, inputError("context must not be nil")
	}
	ids, err := validatePhotoIDs(photoIDs)
	if err != nil {
		return result, err
	}
	result.RequestedPhotoIDs = ids
	if err := ctx.Err(); err != nil {
		return result, contextError(err, false)
	}
	return nativeSetHidden(ctx, ids, hidden, execute)
}

// SetCreationDates sets each photo's creation date, the date Photos sorts
// and shows by. With execute=false it validates the request and returns the
// planned change without mutating Photos.
func SetCreationDates(ctx context.Context, dates []PhotoDate, execute bool) (SetDatesResult, error) {
	result := SetDatesResult{DryRun: !execute}
	if ctx == nil {
		return result, inputError("context must not be nil")
	}
	if len(dates) == 0 {
		return result, inputError("at least one photo date is required")
	}
	seen := make(map[string]bool, len(dates))
	clean := make([]PhotoDate, 0, len(dates))
	for _, entry := range dates {
		if entry.PhotoID == "" {
			return result, inputError("photo IDs must not be empty")
		}
		if entry.Date.IsZero() {
			return result, inputError("every photo needs a date")
		}
		if seen[entry.PhotoID] {
			return result, inputError("a photo ID appears more than once")
		}
		seen[entry.PhotoID] = true
		clean = append(clean, entry)
		result.RequestedPhotoIDs = append(result.RequestedPhotoIDs, entry.PhotoID)
	}
	if err := ctx.Err(); err != nil {
		return result, contextError(err, false)
	}
	return nativeSetCreationDates(ctx, clean, execute)
}

func validatePhotoIDs(photoIDs []string) ([]string, error) {
	if len(photoIDs) == 0 {
		return nil, inputError("at least one photo ID is required")
	}
	seen := make(map[string]struct{}, len(photoIDs))
	ids := make([]string, 0, len(photoIDs))
	for _, id := range photoIDs {
		if id == "" {
			return nil, inputError("photo IDs must not be empty")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

func inputError(message string) error {
	return &Error{Code: "invalid_input", Message: message}
}

func contextError(err error, unknown bool) error {
	code := "cancelled"
	message := "operation was cancelled"
	if errors.Is(err, context.DeadlineExceeded) {
		code = "timeout"
		message = "operation timed out"
	}
	if unknown {
		code = "outcome_unknown"
		message = "PhotoKit accepted the change request, but its outcome is unknown; read the album back before retrying"
	}
	return &Error{Code: code, Message: message, ReadbackRequired: unknown, Cause: err}
}

func unknownRemoveOutcome(result RemoveResult, err error) (RemoveResult, error) {
	result.Executed = true
	result.OutcomeUnknown = true
	return result, withReadbackRequired(err)
}

func unknownSetDatesOutcome(result SetDatesResult, err error) (SetDatesResult, error) {
	result.Executed = true
	result.OutcomeUnknown = true
	return result, withReadbackRequired(err)
}

func unknownSetHiddenOutcome(result SetHiddenResult, err error) (SetHiddenResult, error) {
	result.Executed = true
	result.OutcomeUnknown = true
	return result, withReadbackRequired(err)
}

func withReadbackRequired(err error) error {
	var coded *Error
	if errors.As(err, &coded) {
		copy := *coded
		copy.ReadbackRequired = true
		return &copy
	}
	return &Error{
		Code:             "outcome_unknown",
		Message:          "the PhotoKit result could not be read; inspect the album before retrying",
		ReadbackRequired: true,
		Cause:            err,
	}
}

func nativeError(code, message string) error {
	if code == "" {
		code = "photokit_error"
	}
	if message == "" {
		message = fmt.Sprintf("PhotoKit operation failed (%s)", code)
	}
	return &Error{Code: code, Message: message}
}
