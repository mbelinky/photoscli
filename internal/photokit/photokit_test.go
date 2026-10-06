package photokit

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestRemoveRejectsInvalidInputBeforeNativeAccess(t *testing.T) {
	tests := []struct {
		name  string
		ctx   context.Context
		album string
		ids   []string
	}{
		{name: "nil context", album: "album", ids: []string{"photo"}},
		{name: "empty album", ctx: context.Background(), ids: []string{"photo"}},
		{name: "no photos", ctx: context.Background(), album: "album"},
		{name: "empty photo", ctx: context.Background(), album: "album", ids: []string{"photo", ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := RemoveFromAlbum(tt.ctx, tt.album, tt.ids, false)
			var coded *Error
			if !errors.As(err, &coded) || coded.Code != "invalid_input" {
				t.Fatalf("expected invalid_input, got %v", err)
			}
		})
	}
}

func TestValidatePhotoIDsPreservesExactOrderAndDeduplicates(t *testing.T) {
	got, err := validatePhotoIDs([]string{"photo-b", "photo-a", "photo-b"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"photo-b", "photo-a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestContextErrorsIdentifyUnknownMutationOutcome(t *testing.T) {
	err := contextError(context.DeadlineExceeded, true)
	var coded *Error
	if !errors.As(err, &coded) || coded.Code != "outcome_unknown" || !coded.ReadbackRequired {
		t.Fatalf("unexpected error: %#v", err)
	}
}

func TestUnknownRemoveOutcomeMarksResultAndError(t *testing.T) {
	result, err := unknownRemoveOutcome(RemoveResult{AlbumID: "album"}, nativeError("bridge_error", "decode failed"))
	var coded *Error
	if !result.Executed || !result.OutcomeUnknown {
		t.Fatalf("result did not identify unknown execution outcome: %#v", result)
	}
	if !errors.As(err, &coded) || !coded.ReadbackRequired || coded.Code != "bridge_error" {
		t.Fatalf("error did not require readback: %#v", err)
	}
}

func TestSetHiddenRejectsInvalidInputBeforeNativeAccess(t *testing.T) {
	_, err := SetHidden(context.Background(), []string{""}, true, false)
	var coded *Error
	if !errors.As(err, &coded) || coded.Code != "invalid_input" {
		t.Fatalf("expected invalid_input, got %v", err)
	}
}

func TestSetCreationDatesRejectsInvalidInputBeforeNativeAccess(t *testing.T) {
	ctx := context.Background()
	when := time.Date(2026, 10, 3, 4, 47, 0, 0, time.UTC)
	for name, dates := range map[string][]PhotoDate{
		"empty":     nil,
		"no id":     {{Date: when}},
		"no date":   {{PhotoID: "A/L0/001"}},
		"duplicate": {{PhotoID: "A/L0/001", Date: when}, {PhotoID: "A/L0/001", Date: when}},
	} {
		if _, err := SetCreationDates(ctx, dates, false); err == nil {
			t.Fatalf("%s: invalid input was accepted", name)
		} else if coded, ok := err.(*Error); !ok || coded.Code != "invalid_input" {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
}
