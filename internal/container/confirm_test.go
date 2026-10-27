package container

import "testing"

// Windows' message box has only Yes and No, and Wails runs a button's
// callback only when its label is the one clicked, so there the buttons
// must be named Yes and No or neither answer ever arrives.
func TestQuestionButtonsAreYesAndNoOnWindows(t *testing.T) {
	if yes, no := Buttons("windows", "Continue", "Cancel"); yes != "Yes" || no != "No" {
		t.Fatalf("windows buttons = %q, %q", yes, no)
	}
	if yes, no := Buttons("darwin", "Quit", "Cancel"); yes != "Quit" || no != "Cancel" {
		t.Fatalf("darwin buttons = %q, %q", yes, no)
	}
}
