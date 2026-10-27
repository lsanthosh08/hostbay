package app

import (
	"encoding/json"
	"errors"

	"hostbay/internal/apperr"
)

// MarshalError is the Wails error marshaller. Its output becomes the `cause`
// of the rejected call in the UI: {code, message, details} for an
// *apperr.Error anywhere in the chain, {message} for anything else. Wails'
// default would send {} for both a plain and a wrapped error.
func MarshalError(err error) []byte {
	var e *apperr.Error
	if errors.As(err, &e) {
		b, _ := json.Marshal(e)
		return b
	}
	b, _ := json.Marshal(struct {
		Message string `json:"message"`
	}{err.Error()})
	return b
}

// withHostID names hostID on a SecretNeeded err, so the UI knows whose
// password to ask for, and returns err.
func withHostID(err error, hostID string) error {
	var ae *apperr.Error
	if errors.As(err, &ae) && ae.Code == apperr.SecretNeeded {
		ae.With("hostId", hostID)
	}
	return err
}
