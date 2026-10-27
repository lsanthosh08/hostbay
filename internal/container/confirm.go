package container

import (
	"context"
	"runtime"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// Confirm asks the user, in a dialog Go opens, whether to go ahead with an
// action that reaches past the container (KTD27). The page can ask for the
// action but cannot answer this; true means go ahead.
type Confirm func(ctx context.Context, title, message string) bool

// Buttons are a question dialog's yes and no labels on goos. Windows'
// message box offers only Yes and No, and Wails runs a button's callback
// only when its label is the one clicked.
func Buttons(goos, yes, no string) (string, string) {
	if goos == "windows" {
		return "Yes", "No"
	}
	return yes, no
}

// NativeConfirm shows a native question dialog and waits for its answer.
// No app, or ctx ending first, counts as no.
func NativeConfirm(ctx context.Context, title, message string) bool {
	a := application.Get()
	if a == nil {
		return false
	}
	answer := make(chan bool, 1)
	yes, no := Buttons(runtime.GOOS, "Continue", "Cancel")
	d := a.Dialog.Question().SetTitle(title).SetMessage(message)
	d.AddButton(yes).OnClick(func() { answer <- true })
	cancel := d.AddButton(no)
	cancel.OnClick(func() { answer <- false })
	d.SetDefaultButton(cancel).SetCancelButton(cancel)
	d.Show()
	select {
	case ok := <-answer:
		return ok
	case <-ctx.Done():
		return false
	}
}
