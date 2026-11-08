package planapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gravexa-llc/ai-manager/internal/store"
)

// Error is a refusal: the request can't be done as asked, and Msg says
// why in words a person (or the model behind the mod) can act on. The
// HTTP layer answers with HTTPStatus and {"error": Msg}. Any other error
// from the Service is aimd's own failure (500).
type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

// HTTPStatus is the status the HTTP layer answers with.
func (e *Error) HTTPStatus() int { return e.Status }

func refuse(status int, format string, args ...any) error {
	return &Error{Status: status, Msg: fmt.Sprintf(format, args...)}
}

func badRequest(format string, args ...any) error {
	return refuse(http.StatusBadRequest, format, args...)
}

func notFound(format string, args ...any) error {
	return refuse(http.StatusNotFound, format, args...)
}

// orNotFound turns store.ErrNotFound into a 404 with msg, and passes
// other errors through.
func orNotFound(err error, format string, args ...any) error {
	if errors.Is(err, store.ErrNotFound) {
		return notFound(format, args...)
	}
	return err
}
