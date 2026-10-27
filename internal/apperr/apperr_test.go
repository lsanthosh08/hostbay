package apperr

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestCodeOfFindsWrappedError(t *testing.T) {
	err := fmt.Errorf("dial bastion: %w", New(HostKeyChanged, "host key changed").With("hop", "bastion:22"))
	if got := CodeOf(err); got != HostKeyChanged {
		t.Fatalf("CodeOf = %q, want %q", got, HostKeyChanged)
	}
	if got := CodeOf(errors.New("plain")); got != "" {
		t.Fatalf("CodeOf(plain) = %q, want empty", got)
	}
}

func TestErrorJSONCarriesCodeMessageAndDetails(t *testing.T) {
	e := New(HostKeyUnknown, "unknown host key").With("host", "fe80::1").With("port", "22")
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	details, _ := got["details"].(map[string]any)
	if got["code"] != "HostKeyUnknown" || got["message"] != "unknown host key" || details["host"] != "fe80::1" {
		t.Fatalf("json = %s", b)
	}
}
