package infer

import (
	"fmt"
	"testing"
)

// A request with too many problems is as refused as one with a few.
func TestTooManyProblemsIsStillARefusal(t *testing.T) {
	c := &checker{}
	for i := range MaxValidationIssues * 2 {
		c.fail("p", fmt.Sprint(i))
	}
	if c.ok() || len(c.issues) != MaxValidationIssues {
		t.Errorf("ok %v, %d kept", c.ok(), len(c.issues))
	}
}

// Clients can see these, and the documents state them.
func TestTheLimitsAreTheStatedOnes(t *testing.T) {
	if MaxValidationIssues != 100 || MaxPathKey != 64 || MaxListedKeys != 20 {
		t.Errorf("%d issues, %d bytes of a key, %d unrecognized keys", MaxValidationIssues, MaxPathKey, MaxListedKeys)
	}
}
