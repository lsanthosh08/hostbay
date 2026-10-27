package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"

	"hostbay/internal/apperr"
)

// uiError decodes what the UI receives as the rejected call's `cause`:
// Wails wraps MarshalError's output in a CallError.
func uiError(t *testing.T, err error) map[string]any {
	t.Helper()
	b, jerr := json.Marshal(&application.CallError{
		Message: err.Error(),
		Cause:   json.RawMessage(MarshalError(err)),
		Kind:    application.RuntimeError,
	})
	if jerr != nil {
		t.Fatal(jerr)
	}
	var wire struct {
		Cause map[string]any `json:"cause"`
	}
	if jerr := json.Unmarshal(b, &wire); jerr != nil {
		t.Fatal(jerr)
	}
	return wire.Cause
}

func TestMarshalErrorSendsCodeMessageAndDetails(t *testing.T) {
	err := fmt.Errorf("connect: %w", apperr.New(apperr.Unreachable, "web1 did not answer").With("host", "web1"))
	got := uiError(t, err)
	details, _ := got["details"].(map[string]any)
	if got["code"] != "Unreachable" || got["message"] != "web1 did not answer" || details["host"] != "web1" {
		t.Fatalf("cause = %v", got)
	}
}

func TestMarshalErrorSendsPlainErrorAsMessageOnly(t *testing.T) {
	got := uiError(t, errors.New("disk full"))
	if got["message"] != "disk full" {
		t.Fatalf("cause = %v", got)
	}
	if _, ok := got["code"]; ok {
		t.Fatalf("plain error carries a code: %v", got)
	}
}
