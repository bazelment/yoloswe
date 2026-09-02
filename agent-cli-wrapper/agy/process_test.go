package agy

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildCLIArgs_DefaultPrintMode(t *testing.T) {
	t.Parallel()

	pm := newProcessManager("hello", defaultConfig())

	assert.Equal(t, []string{"-p", "hello"}, pm.BuildCLIArgs())
}

func TestBuildCLIArgs_AllOptions(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	WithModel("gemini-3.8-flash-low")(&cfg)
	WithEffort("low")(&cfg)
	WithPrintTimeout(2 * time.Minute)(&cfg)
	WithConversation("conv-123")(&cfg)
	WithLogFile("/tmp/agy.log")(&cfg)
	WithAddDir("/tmp/extra")(&cfg)
	WithDangerouslySkipPermissions()(&cfg)
	WithSandbox()(&cfg)
	WithExtraArgs("--future-flag")(&cfg)

	pm := newProcessManager("hello", cfg)

	assert.Equal(t, []string{
		"--model", "gemini-3.8-flash-low",
		"--effort", "low",
		"--print-timeout", "120s",
		"--conversation", "conv-123",
		"--log-file", "/tmp/agy.log",
		"--add-dir", "/tmp/extra",
		"--dangerously-skip-permissions",
		"--sandbox",
		"--future-flag",
		"-p", "hello",
	}, pm.BuildCLIArgs())
}

func TestBuildCLIArgs_FlagsPrecedePrintFlag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts []SessionOption
	}{
		{
			name: "model only",
			opts: []SessionOption{WithModel("gemini-3.8-flash-low")},
		},
		{
			name: "effort only",
			opts: []SessionOption{WithEffort("high")},
		},
		{
			name: "model and effort",
			opts: []SessionOption{
				WithModel("claude-sonnet-4-6"),
				WithEffort("medium"),
			},
		},
		{
			name: "model, effort, and extra args",
			opts: []SessionOption{
				WithModel("gpt-oss-120b-medium"),
				WithEffort("low"),
				WithExtraArgs("--future-flag", "value"),
			},
		},
		{
			name: "extra args alone must not trail -p",
			opts: []SessionOption{WithExtraArgs("--sandbox")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := defaultConfig()
			for _, opt := range tt.opts {
				opt(&cfg)
			}
			pm := newProcessManager("the prompt", cfg)
			args := pm.BuildCLIArgs()

			pIdx := slices.Index(args, "-p")
			require.GreaterOrEqualf(t, pIdx, 0, "-p flag missing from args: %v", args)
			require.Lessf(t, pIdx, len(args)-1, "-p has no trailing prompt token: %v", args)
			require.Equalf(t, "the prompt", args[pIdx+1], "-p must be immediately followed by the prompt: %v", args)

			for _, flag := range []string{"--model", "--effort", "--future-flag", "--sandbox"} {
				if idx := slices.Index(args, flag); idx >= 0 {
					assert.Lessf(t, idx, pIdx, "%s at %d must precede -p at %d: %v", flag, idx, pIdx, args)
				}
			}

			// -p and its prompt must be the final two tokens: nothing may
			// follow the prompt, and nothing between a flag and -p is a flag
			// value belonging to -p.
			assert.Equal(t, len(args)-2, pIdx, "-p <prompt> must be the trailing pair: %v", args)
		})
	}
}

// TestBuildCLIArgs_NoRegressionToLeadingPrintFlag guards against the old
// ordering (-p <prompt> emitted first, with flags such as --model appended
// after) ever coming back. It pins the exact slice rather than just probing
// args[0]: an args[0] check alone still passes for orderings that place a
// flag after the prompt, which is the regression it is named for.
func TestBuildCLIArgs_NoRegressionToLeadingPrintFlag(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	WithModel("gemini-3.8-flash-low")(&cfg)
	WithEffort("high")(&cfg)

	pm := newProcessManager("hello", cfg)

	assert.Equal(t, []string{
		"--model", "gemini-3.8-flash-low",
		"--effort", "high",
		"-p", "hello",
	}, pm.BuildCLIArgs())
}

func TestBuildCLIArgs_EmptyModelAndEffortEmitNothing(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	WithModel("")(&cfg)
	WithEffort("")(&cfg)

	pm := newProcessManager("hello", cfg)
	args := pm.BuildCLIArgs()

	assert.Equal(t, -1, slices.Index(args, "--model"))
	assert.Equal(t, -1, slices.Index(args, "--effort"))
	assert.Equal(t, []string{"-p", "hello"}, args)
}
