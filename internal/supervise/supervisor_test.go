package supervise

import (
	"testing"
	"time"
)

func TestRecoveryPolicyBounds(t *testing.T) {
	if syncFreezeProbeDelay != 30*time.Second || maxRecoveryCycles != 5 {
		t.Fatal("recovery requires 30s observations and at most five cycles without progress")
	}
}
