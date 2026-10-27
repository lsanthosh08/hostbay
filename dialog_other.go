//go:build !windows

package main

import "github.com/wailsapp/wails/v3/pkg/application"

// choose shows a native warning with one button per label and waits for
// the answer. The first label is the default, the last answers Escape.
func choose(title, message string, labels ...string) string {
	answer := make(chan string, 1)
	d := application.Get().Dialog.Warning().SetTitle(title).SetMessage(message)
	var buttons []*application.Button
	for _, l := range labels {
		buttons = append(buttons, d.AddButton(l).OnClick(func() { answer <- l }))
	}
	d.SetDefaultButton(buttons[0]).SetCancelButton(buttons[len(buttons)-1])
	d.Show()
	return <-answer
}
