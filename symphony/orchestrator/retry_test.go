package orchestrator

import "testing"

func TestBackoffDelayMs(t *testing.T) {
	t.Parallel()

	maxBackoff := 300000 // 5 minutes

	tests := []struct {
		attempt int
		want    int64
	}{
		{1, 10000},  // 10s
		{2, 20000},  // 20s
		{3, 40000},  // 40s
		{4, 80000},  // 80s
		{5, 160000}, // 160s
		{6, 300000}, // capped at 5m
		{7, 300000}, // still capped
		{0, 10000},  // attempt < 1 treated as 1
	}

	for _, tt := range tests {
		got := backoffDelayMs(tt.attempt, maxBackoff)
		if got != tt.want {
			t.Errorf("backoffDelayMs(%d, %d) = %d, want %d", tt.attempt, maxBackoff, got, tt.want)
		}
	}
}

func TestBackoffDelayMs_CustomMax(t *testing.T) {
	t.Parallel()

	// With a low max, it caps early.
	got := backoffDelayMs(3, 15000)
	if got != 15000 {
		t.Errorf("backoffDelayMs(3, 15000) = %d, want 15000", got)
	}
}
