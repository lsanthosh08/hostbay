package main

import (
	"fmt"
	"strings"
)

// Windows message box answers (IDCANCEL, IDYES, IDNO, IDTRYAGAIN, IDCONTINUE).
const (
	idCancel   = 2
	idYes      = 6
	idNo       = 7
	idTryAgain = 10
	idContinue = 11
)

// chooseByBoxes offers three or four labels through Windows message boxes,
// which have at most three buttons. box shows text with Try Again, Continue
// and Cancel, or with Yes and No when yesNo is set, and returns the answer.
// Try Again is the first label and Cancel the last; with four labels,
// Continue asks a second Yes/No box for the second or the third.
func chooseByBoxes(message string, labels []string, box func(text string, yesNo bool) int) string {
	mid := labels[1 : len(labels)-1]
	msg := fmt.Sprintf("%s\n\nTry Again: %s\nContinue: %s\nCancel: %s", message, labels[0], strings.Join(mid, " or "), labels[len(labels)-1])
	switch box(msg, false) {
	case idTryAgain:
		return labels[0]
	case idContinue:
		if len(mid) == 1 {
			return mid[0]
		}
		if box(fmt.Sprintf("Yes: %s\nNo: %s", mid[0], mid[1]), true) == idYes {
			return mid[0]
		}
		return mid[1]
	}
	return labels[len(labels)-1]
}
