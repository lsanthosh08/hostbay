package main

import (
	"strings"
	"testing"
)

// A Windows message box has at most three buttons, so a fourth label must
// still be reachable: every label of the missing-folder prompt can be
// picked, and the boxes name each one.
func TestEveryLabelIsReachableThroughWindowsBoxes(t *testing.T) {
	labels := []string{"Retry", "Choose another folder", "Use the default folder", "Quit"}
	for _, c := range []struct {
		answers []int
		want    string
	}{
		{[]int{idTryAgain}, "Retry"},
		{[]int{idContinue, idYes}, "Choose another folder"},
		{[]int{idContinue, idNo}, "Use the default folder"},
		{[]int{idCancel}, "Quit"},
	} {
		var shown []string
		got := chooseByBoxes("The data folder is gone.", labels, func(text string, yesNo bool) int {
			shown = append(shown, text)
			if yesNo != (len(shown) == 2) {
				t.Fatalf("box %d: yesNo = %v", len(shown), yesNo)
			}
			return c.answers[len(shown)-1]
		})
		if got != c.want || len(shown) != len(c.answers) {
			t.Errorf("answers %v chose %q after %d boxes, want %q", c.answers, got, len(shown), c.want)
		}
		if !strings.Contains(shown[0], labels[2]) {
			t.Errorf("the first box does not name %q", labels[2])
		}
	}
	three := []string{"Retry", "Choose another folder", "Quit"}
	if got := chooseByBoxes("gone", three, func(string, bool) int { return idContinue }); got != "Choose another folder" {
		t.Errorf("three labels: Continue chose %q", got)
	}
}
