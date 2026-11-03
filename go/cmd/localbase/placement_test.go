package main

import (
	"strings"
	"testing"
)

// The real 16 GB Linux runner: the line above a next-load refusal said "Fits, 11 GB to
// spare" while admission refused.
func TestFitLineUsesAdmissionMath(t *testing.T) {
	got := fitLine(8_266_688_480, 16_759_390_208, 8_589_934_592)
	if !strings.Contains(got, "Does not fit") || !strings.Contains(got, "needs 7.7 GB") || !strings.Contains(got, "keeping 8.0 GB") {
		t.Errorf("%s", got)
	}
	got = fitLine(1<<30, 64<<30, 8<<30)
	if !strings.Contains(got, "Fits, 55.0 GB to spare of 64.0 GB after keeping 8.0 GB") {
		t.Errorf("%s", got)
	}
}
