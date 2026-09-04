package supervise

import (
	"testing"
	"time"
)

func TestLockRetryPolicy(t *testing.T) {
	if maxConsecutiveLockErrors != 3 {
		t.Fatalf("diagnosis should begin on the third consecutive lock error, got %d", maxConsecutiveLockErrors)
	}

	tests := []struct {
		name        string
		consecutive int
		want        time.Duration
	}{
		{name: "no error", consecutive: 0, want: 0},
		{name: "first error", consecutive: 1, want: 10 * time.Second},
		{name: "second error", consecutive: 2, want: 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := lockRetryDelay(tt.consecutive)
			if got != tt.want {
				t.Fatalf("lockRetryDelay(%d) = %s, want %s", tt.consecutive, got, tt.want)
			}
			if got > maxLockRetryDelay {
				t.Fatalf("lockRetryDelay(%d) = %s, exceeds %s ceiling", tt.consecutive, got, maxLockRetryDelay)
			}
		})
	}
}
