//go:build darwin && cgo

package photokit

import "testing"

func TestNativeRemoveResultEncodesDryRunAsJSONBoolean(t *testing.T) {
	for _, execute := range []bool{false, true} {
		response, err := nativeRemoveResultEncodingProbe(execute)
		if err != nil {
			t.Fatalf("execute=%v: %v", execute, err)
		}
		if response.DryRun != !execute {
			t.Fatalf("execute=%v: DryRun=%v", execute, response.DryRun)
		}
		if response.Executed || response.Verified {
			t.Fatalf("probe unexpectedly represented a mutation: %#v", response.removeResult())
		}
	}
}

func TestNativeSetHiddenResultEncodesDryRunAsJSONBoolean(t *testing.T) {
	for _, execute := range []bool{false, true} {
		response, err := nativeSetHiddenResultEncodingProbe(execute)
		if err != nil {
			t.Fatalf("execute=%v: %v", execute, err)
		}
		if response.DryRun != !execute || response.Executed || response.Verified || !response.Hidden {
			t.Fatalf("unexpected probe: %#v", response.setHiddenResult())
		}
	}
}

func TestRemoveErrorReadbackRequirement(t *testing.T) {
	tests := []struct {
		name     string
		response nativeResponse
		want     bool
	}{
		{
			name:     "verification failed after execution",
			response: nativeResponse{Code: "verification_failed", Executed: true},
			want:     true,
		},
		{
			name:     "native exception after submission",
			response: nativeResponse{Code: "photokit_exception", Executed: true},
			want:     true,
		},
		{
			name:     "PhotoKit transaction failed atomically",
			response: nativeResponse{Code: "change_failed", Executed: true},
			want:     false,
		},
		{
			name:     "change request rejected before application",
			response: nativeResponse{Code: "change_rejected", Executed: true},
			want:     false,
		},
		{
			name:     "preflight failure",
			response: nativeResponse{Code: "photos_not_in_album", Executed: false},
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := removeError(tt.response).ReadbackRequired; got != tt.want {
				t.Fatalf("ReadbackRequired=%v, want %v", got, tt.want)
			}
		})
	}
}
