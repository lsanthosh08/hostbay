package main

import (
	"golang.org/x/sys/windows"
)

// choose shows a native warning offering three or four labels and waits
// for the answer. Wails' Windows message box offers only Yes and No, so the
// labels map onto Windows' own boxes as chooseByBoxes describes, and the
// text names each.
func choose(title, message string, labels ...string) string {
	return chooseByBoxes(message, labels, func(text string, yesNo bool) int {
		var flags uint32 = windows.MB_CANCELTRYCONTINUE | windows.MB_ICONWARNING
		if yesNo {
			flags = windows.MB_YESNO | windows.MB_ICONQUESTION
		}
		r, _ := windows.MessageBox(0, windows.StringToUTF16Ptr(text), windows.StringToUTF16Ptr(title), flags|windows.MB_SETFOREGROUND)
		return int(r)
	})
}
