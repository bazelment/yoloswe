package main

import "testing"

func TestTruncateStr(t *testing.T) {
	t.Parallel()

	if got := truncateStr("short", 10); got != "short" {
		t.Fatalf("truncateStr(short) = %q", got)
	}
	if got := truncateStr("abcdefghijklmnopqrstuvwxyz", 10); got != "abcdefg..." {
		t.Fatalf("truncateStr(long) = %q, want abcdefg...", got)
	}
	if got := truncateStr("abcdef", 2); got != "..." {
		t.Fatalf("truncateStr(max<3) = %q, want ...", got)
	}
}
