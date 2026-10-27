package database

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"hostbay/internal/apperr"
)

// A driver error that echoes the password reaches the page without it,
// with its category; a coded error keeps its code and loses the password
// from its details too.
func TestAppErrorScrubsThePassword(t *testing.T) {
	const pw = "s3cret-Pw"
	err := fmt.Errorf("query: %w", &Error{Category: "auth", Err: errors.New("login as u with " + pw + " failed")})
	ae := AppError(err, pw).(*apperr.Error)
	if ae.Code != apperr.DatabaseError || ae.Details["category"] != "auth" || strings.Contains(ae.Message+ae.Details["detail"], pw) || !strings.Contains(ae.Message, "failed") {
		t.Fatalf("err = %+v", ae)
	}
	coded := apperr.New(apperr.SecretNeeded, "refused "+pw).With("why", pw)
	ae = AppError(coded, pw).(*apperr.Error)
	if ae.Code != apperr.SecretNeeded || strings.Contains(ae.Message+ae.Details["why"], pw) {
		t.Fatalf("err = %+v", ae)
	}
	if c := Category(context.Canceled); c != "cancelled" {
		t.Fatalf("category = %q", c)
	}
}
