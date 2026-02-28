package cursor

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildCLIArgs_Default(t *testing.T) {
	pm := newProcessManager("hello world", defaultConfig())
	args := pm.BuildCLIArgs()

	expected := []string{
		"chat",
		"-p", "hello world",
		"--output-format", "stream-json",
	}
	assert.Equal(t, expected, args)
}

func TestBuildCLIArgs_WithModel(t *testing.T) {
	config := defaultConfig()
	config.Model = "cursor-fast"
	pm := newProcessManager("test", config)
	args := pm.BuildCLIArgs()

	assert.Contains(t, args, "--model")
	assert.Contains(t, args, "cursor-fast")
}

func TestBuildCLIArgs_WithAllFlags(t *testing.T) {
	config := defaultConfig()
	config.Model = "cursor-fast"
	config.Force = true
	config.Trust = true
	config.Sandbox = true
	pm := newProcessManager("test prompt", config)
	args := pm.BuildCLIArgs()

	assert.Contains(t, args, "--model")
	assert.Contains(t, args, "cursor-fast")
	assert.Contains(t, args, "--force")
	assert.Contains(t, args, "--trust")
	assert.Contains(t, args, "--sandbox")
	// Verify prompt is in args
	assert.Contains(t, args, "test prompt")
}

func TestBuildCLIArgs_WithExtraArgs(t *testing.T) {
	config := defaultConfig()
	config.ExtraArgs = []string{"--verbose", "--debug"}
	pm := newProcessManager("test", config)
	args := pm.BuildCLIArgs()

	assert.Contains(t, args, "--verbose")
	assert.Contains(t, args, "--debug")
}

func TestBuildCLIArgs_PromptWithSpaces(t *testing.T) {
	pm := newProcessManager("write a function that adds two numbers", defaultConfig())
	args := pm.BuildCLIArgs()

	// The prompt should be a single argument
	assert.Equal(t, "write a function that adds two numbers", args[2])
}
