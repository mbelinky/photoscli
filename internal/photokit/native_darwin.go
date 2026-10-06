//go:build darwin && cgo

package photokit

/*
#cgo CFLAGS: -fobjc-arc -fblocks
#cgo LDFLAGS: -framework Foundation -framework Photos
#include <stdlib.h>
#include "photokit.h"
*/
import "C"

import (
	"context"
	"encoding/json"
	"time"
	"unsafe"
)

type nativeResponse struct {
	OK                     bool                `json:"ok"`
	Code                   string              `json:"code"`
	Message                string              `json:"message"`
	Status                 AuthorizationStatus `json:"status"`
	AuthorizationStatus    AuthorizationStatus `json:"authorizationStatus"`
	DryRun                 bool                `json:"dryRun"`
	Executed               bool                `json:"executed"`
	Verified               bool                `json:"verified"`
	OutcomeUnknown         bool                `json:"outcomeUnknown"`
	AlbumID                string              `json:"albumId"`
	AlbumTitle             string              `json:"albumTitle"`
	Hidden                 bool                `json:"hidden"`
	RequestedPhotoIDs      []string            `json:"requestedPhotoIds"`
	WouldRemovePhotoIDs    []string            `json:"wouldRemovePhotoIds"`
	RemovedPhotoIDs        []string            `json:"removedPhotoIds"`
	MissingPhotoIDs        []string            `json:"missingPhotoIds"`
	NotAlbumMemberPhotoIDs []string            `json:"notAlbumMembersPhotoIds"`
	StillInAlbumPhotoIDs   []string            `json:"stillInAlbumPhotoIds"`
	MissingPriorMemberIDs  []string            `json:"missingPriorMemberPhotoIds"`
	MissingFromLibraryIDs  []string            `json:"missingFromLibraryPhotoIds"`
	BeforeMemberCount      int                 `json:"beforeMemberCount"`
	AfterMemberCount       int                 `json:"afterMemberCount"`
	WouldChangePhotoIDs    []string            `json:"wouldChangePhotoIds"`
	AlreadyInStatePhotoIDs []string            `json:"alreadyInStatePhotoIds"`
	ChangedPhotoIDs        []string            `json:"changedPhotoIds"`
	NotInStatePhotoIDs     []string            `json:"notInStatePhotoIds"`
}

func (r nativeResponse) removeResult() RemoveResult {
	return RemoveResult{DryRun: r.DryRun, Executed: r.Executed, Verified: r.Verified, OutcomeUnknown: r.OutcomeUnknown, AlbumID: r.AlbumID, AlbumTitle: r.AlbumTitle, RequestedPhotoIDs: r.RequestedPhotoIDs, WouldRemovePhotoIDs: r.WouldRemovePhotoIDs, RemovedPhotoIDs: r.RemovedPhotoIDs, MissingPhotoIDs: r.MissingPhotoIDs, NotAlbumMemberPhotoIDs: r.NotAlbumMemberPhotoIDs, StillInAlbumPhotoIDs: r.StillInAlbumPhotoIDs, MissingPriorMemberIDs: r.MissingPriorMemberIDs, MissingFromLibraryIDs: r.MissingFromLibraryIDs, BeforeMemberCount: r.BeforeMemberCount, AfterMemberCount: r.AfterMemberCount}
}
func (r nativeResponse) setDatesResult() SetDatesResult {
	return SetDatesResult{DryRun: r.DryRun, Executed: r.Executed, Verified: r.Verified, OutcomeUnknown: r.OutcomeUnknown, RequestedPhotoIDs: r.RequestedPhotoIDs, WouldChangePhotoIDs: r.WouldChangePhotoIDs, AlreadyInStatePhotoIDs: r.AlreadyInStatePhotoIDs, ChangedPhotoIDs: r.ChangedPhotoIDs, MissingPhotoIDs: r.MissingPhotoIDs, NotInStatePhotoIDs: r.NotInStatePhotoIDs, MissingFromLibraryPhotoIDs: r.MissingFromLibraryIDs}
}

func nativeSetCreationDates(ctx context.Context, dates []PhotoDate, execute bool) (SetDatesResult, error) {
	base := SetDatesResult{DryRun: !execute}
	cIDs := make([]*C.char, len(dates))
	seconds := make([]C.double, len(dates))
	for i, entry := range dates {
		base.RequestedPhotoIDs = append(base.RequestedPhotoIDs, entry.PhotoID)
		cIDs[i] = C.CString(entry.PhotoID)
		defer C.free(unsafe.Pointer(cIDs[i]))
		seconds[i] = C.double(float64(entry.Date.UnixNano()) / 1e9)
	}
	request := C.PKStartSetCreationDates((**C.char)(unsafe.Pointer(&cIDs[0])), (*C.double)(unsafe.Pointer(&seconds[0])), C.size_t(len(cIDs)), boolInt(execute))
	if request == nil {
		return base, nativeError("bridge_error", "could not start the PhotoKit creation-date request")
	}
	defer C.PKRequestRelease(request)
	response, err := waitForResponse(ctx, request, execute)
	if err != nil {
		if execute {
			return unknownSetDatesOutcome(base, err)
		}
		return base, err
	}
	if !response.OK {
		err := setHiddenError(response)
		if err.ReadbackRequired {
			response.OutcomeUnknown = true
		}
		return response.setDatesResult(), err
	}
	return response.setDatesResult(), nil
}

func (r nativeResponse) setHiddenResult() SetHiddenResult {
	return SetHiddenResult{DryRun: r.DryRun, Executed: r.Executed, Verified: r.Verified, OutcomeUnknown: r.OutcomeUnknown, Hidden: r.Hidden, RequestedPhotoIDs: r.RequestedPhotoIDs, WouldChangePhotoIDs: r.WouldChangePhotoIDs, AlreadyInStatePhotoIDs: r.AlreadyInStatePhotoIDs, ChangedPhotoIDs: r.ChangedPhotoIDs, MissingPhotoIDs: r.MissingPhotoIDs, NotInStatePhotoIDs: r.NotInStatePhotoIDs, MissingFromLibraryPhotoIDs: r.MissingFromLibraryIDs}
}

func nativeAuthStatus() (AuthorizationStatus, error) {
	value := C.PKCopyAuthorizationStatusJSON()
	if value == nil {
		return AuthorizationUnknown, nativeError("bridge_error", "PhotoKit returned no authorization result")
	}
	defer C.PKFreeString(value)
	var response nativeResponse
	if err := json.Unmarshal([]byte(C.GoString(value)), &response); err != nil {
		return AuthorizationUnknown, &Error{Code: "bridge_error", Message: "could not decode the PhotoKit authorization result", Cause: err}
	}
	if !response.OK {
		return response.Status, nativeError(response.Code, response.Message)
	}
	return response.Status, nil
}

func nativeRemoveResultEncodingProbe(execute bool) (nativeResponse, error) {
	value := C.PKCopyRemoveResultEncodingProbeJSON(boolInt(execute))
	if value == nil {
		return nativeResponse{}, nativeError("bridge_error", "PhotoKit returned no encoding probe result")
	}
	defer C.PKFreeString(value)
	var response nativeResponse
	if err := json.Unmarshal([]byte(C.GoString(value)), &response); err != nil {
		return nativeResponse{}, &Error{Code: "bridge_error", Message: "could not decode the PhotoKit encoding probe", Cause: err}
	}
	return response, nil
}

func nativeSetHiddenResultEncodingProbe(execute bool) (nativeResponse, error) {
	value := C.PKCopySetHiddenResultEncodingProbeJSON(boolInt(execute))
	if value == nil {
		return nativeResponse{}, nativeError("bridge_error", "PhotoKit returned no encoding probe result")
	}
	defer C.PKFreeString(value)
	var response nativeResponse
	if err := json.Unmarshal([]byte(C.GoString(value)), &response); err != nil {
		return nativeResponse{}, &Error{Code: "bridge_error", Message: "could not decode the PhotoKit encoding probe", Cause: err}
	}
	return response, nil
}

func nativeRequestAuthorization(ctx context.Context) (AuthorizationStatus, error) {
	request := C.PKStartAuthorizationRequest()
	if request == nil {
		return AuthorizationUnknown, nativeError("bridge_error", "could not start the PhotoKit authorization request")
	}
	defer C.PKRequestRelease(request)
	response, err := waitForResponse(ctx, request, false)
	if err != nil {
		return AuthorizationUnknown, err
	}
	if !response.OK {
		return response.Status, nativeError(response.Code, response.Message)
	}
	return response.Status, nil
}

func nativeRemoveFromAlbum(ctx context.Context, albumID string, photoIDs []string, execute bool) (RemoveResult, error) {
	base := RemoveResult{
		DryRun:            !execute,
		AlbumID:           albumID,
		RequestedPhotoIDs: append([]string(nil), photoIDs...),
	}
	album := C.CString(albumID)
	defer C.free(unsafe.Pointer(album))
	cIDs := make([]*C.char, len(photoIDs))
	for i, id := range photoIDs {
		cIDs[i] = C.CString(id)
		defer C.free(unsafe.Pointer(cIDs[i]))
	}
	request := C.PKStartRemoveFromAlbum(album, (**C.char)(unsafe.Pointer(&cIDs[0])), C.size_t(len(cIDs)), boolInt(execute))
	if request == nil {
		return base, nativeError("bridge_error", "could not start the PhotoKit album request")
	}
	defer C.PKRequestRelease(request)
	response, err := waitForResponse(ctx, request, execute)
	if err != nil {
		if execute {
			return unknownRemoveOutcome(base, err)
		}
		return base, err
	}
	if !response.OK {
		err := removeError(response)
		if err.ReadbackRequired {
			response.OutcomeUnknown = true
		}
		return response.removeResult(), err
	}
	return response.removeResult(), nil
}

func nativeSetHidden(ctx context.Context, photoIDs []string, hidden, execute bool) (SetHiddenResult, error) {
	base := SetHiddenResult{DryRun: !execute, Hidden: hidden, RequestedPhotoIDs: append([]string(nil), photoIDs...)}
	cIDs := make([]*C.char, len(photoIDs))
	for i, id := range photoIDs {
		cIDs[i] = C.CString(id)
		defer C.free(unsafe.Pointer(cIDs[i]))
	}
	request := C.PKStartSetHidden((**C.char)(unsafe.Pointer(&cIDs[0])), C.size_t(len(cIDs)), boolInt(hidden), boolInt(execute))
	if request == nil {
		return base, nativeError("bridge_error", "could not start the PhotoKit hidden-state request")
	}
	defer C.PKRequestRelease(request)
	response, err := waitForResponse(ctx, request, execute)
	if err != nil {
		if execute {
			return unknownSetHiddenOutcome(base, err)
		}
		return base, err
	}
	if !response.OK {
		err := setHiddenError(response)
		if err.ReadbackRequired {
			response.OutcomeUnknown = true
		}
		return response.setHiddenResult(), err
	}
	return response.setHiddenResult(), nil
}

func removeError(response nativeResponse) *Error {
	err := nativeError(response.Code, response.Message).(*Error)
	details := map[string]any{
		"albumId": response.AlbumID,
	}
	if response.AuthorizationStatus != "" {
		details["authorizationStatus"] = response.AuthorizationStatus
	}
	for key, ids := range map[string][]string{
		"missingPhotoIds":            response.MissingPhotoIDs,
		"notAlbumMembersPhotoIds":    response.NotAlbumMemberPhotoIDs,
		"stillInAlbumPhotoIds":       response.StillInAlbumPhotoIDs,
		"missingPriorMemberPhotoIds": response.MissingPriorMemberIDs,
		"missingFromLibraryPhotoIds": response.MissingFromLibraryIDs,
	} {
		if len(ids) > 0 {
			details[key] = ids
		}
	}
	err.Details = details
	if response.Executed && !response.Verified && !conclusivelyNotApplied(response.Code) {
		err.ReadbackRequired = true
	}
	return err
}

func setHiddenError(response nativeResponse) *Error {
	err := nativeError(response.Code, response.Message).(*Error)
	details := make(map[string]any)
	if response.AuthorizationStatus != "" {
		details["authorizationStatus"] = response.AuthorizationStatus
	}
	for key, ids := range map[string][]string{
		"missingPhotoIds": response.MissingPhotoIDs, "notInStatePhotoIds": response.NotInStatePhotoIDs,
		"missingFromLibraryPhotoIds": response.MissingFromLibraryIDs,
	} {
		if len(ids) > 0 {
			details[key] = ids
		}
	}
	err.Details = details
	if response.Executed && !response.Verified && !conclusivelyNotApplied(response.Code) {
		err.ReadbackRequired = true
	}
	return err
}

func conclusivelyNotApplied(code string) bool {
	switch code {
	case "change_rejected", "change_failed":
		return true
	default:
		return false
	}
}

func waitForResponse(ctx context.Context, request *C.PKRequest, mutationStarted bool) (nativeResponse, error) {
	for {
		// Read a synchronously completed preflight before observing cancellation.
		// This prevents an invalid request from being reported as an unknown
		// mutation merely because its context expired at the same instant.
		if done := C.PKRequestWait(request, 0); done == 1 {
			return copyResponse(request)
		} else if done < 0 {
			return nativeResponse{}, nativeError("bridge_error", "PhotoKit request handle became invalid")
		}
		if err := ctx.Err(); err != nil {
			return nativeResponse{}, contextError(err, mutationStarted)
		}
		wait := 100 * time.Millisecond
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return nativeResponse{}, contextError(context.DeadlineExceeded, mutationStarted)
			}
			if remaining < wait {
				wait = remaining
			}
		}
		milliseconds := int(wait / time.Millisecond)
		if milliseconds < 1 {
			milliseconds = 1
		}
		done := C.PKRequestWait(request, C.int(milliseconds))
		if done < 0 {
			return nativeResponse{}, nativeError("bridge_error", "PhotoKit request handle became invalid")
		}
		if done == 1 {
			return copyResponse(request)
		}
		select {
		case <-ctx.Done():
			return nativeResponse{}, contextError(ctx.Err(), mutationStarted)
		default:
		}
	}
}

func copyResponse(request *C.PKRequest) (nativeResponse, error) {
	value := C.PKRequestCopyResultJSON(request)
	if value == nil {
		return nativeResponse{}, nativeError("bridge_error", "PhotoKit completed without a result")
	}
	defer C.PKFreeString(value)
	var response nativeResponse
	if err := json.Unmarshal([]byte(C.GoString(value)), &response); err != nil {
		return nativeResponse{}, &Error{Code: "bridge_error", Message: "could not decode the PhotoKit result", Cause: err}
	}
	return response, nil
}

func boolInt(value bool) C.int {
	if value {
		return 1
	}
	return 0
}
