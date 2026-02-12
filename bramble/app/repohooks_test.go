package app

import (
	"testing"
)

func TestRunRepoHookCommandsSuccess(t *testing.T) {
	tmp := t.TempDir()
	var messages []string
	err := runRepoHookCommands([]string{"echo ok > /dev/null"}, tmp, "feature/test", &messages)
	if err != nil {
		t.Fatalf("runRepoHookCommands() error = %v", err)
	}
	if len(messages) == 0 {
		t.Fatal("expected messages to include executed command")
	}
}

func TestRunRepoHookCommandsFailure(t *testing.T) {
	tmp := t.TempDir()
	var messages []string
	err := runRepoHookCommands([]string{"false"}, tmp, "feature/test", &messages)
	if err == nil {
		t.Fatal("expected error for failing command")
	}
	if len(messages) < 2 {
		t.Fatalf("expected running+failed messages, got %v", messages)
	}
}
