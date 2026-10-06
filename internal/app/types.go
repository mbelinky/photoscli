package app

import "context"

type Album struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	PhotoCount *int     `json:"photoCount,omitempty"`
	PhotoIDs   []string `json:"photoIds,omitempty"`
}

type Backend interface {
	ListAlbums(context.Context) ([]Album, error)
	GetAlbum(context.Context, string) (Album, error)
	FindPhotoIDs(context.Context, []string) ([]string, error)
	CreateAlbum(context.Context, string) (Album, error)
	AddPhotos(context.Context, string, []string) error
	ImportFiles(context.Context, string, []string, bool) ([]string, error)
	Probe(context.Context) (int, error)
}

type CodedError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *CodedError) Error() string { return e.Message }

func errCode(code, message string, details map[string]any) error {
	return &CodedError{Code: code, Message: message, Details: details}
}
