package agy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestBuildCLIArgs_DefaultPrintMode(t *testing.T) {
	t.Parallel()

	pm := newProcessManager("hello", defaultConfig())

	assert.Equal(t, []string{"-p", "hello"}, pm.BuildCLIArgs())
}

func TestBuildCLIArgs_AllOptions(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	WithPrintTimeout(2 * time.Minute)(&cfg)
	WithConversation("conv-123")(&cfg)
	WithLogFile("/tmp/agy.log")(&cfg)
	WithAddDir("/tmp/extra")(&cfg)
	WithDangerouslySkipPermissions()(&cfg)
	WithSandbox()(&cfg)
	WithExtraArgs("--future-flag")(&cfg)

	pm := newProcessManager("hello", cfg)

	assert.Equal(t, []string{
		"-p", "hello",
		"--print-timeout", "120s",
		"--conversation", "conv-123",
		"--log-file", "/tmp/agy.log",
		"--add-dir", "/tmp/extra",
		"--dangerously-skip-permissions",
		"--sandbox",
		"--future-flag",
	}, pm.BuildCLIArgs())
}
