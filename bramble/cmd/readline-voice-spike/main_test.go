package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWriteVoicePartial(t *testing.T) {
	t.Parallel()

	var out strings.Builder

	writeVoicePartial(&out, "hello world")

	require.Equal(t, "\0337\n\033[K\033[2m[voice] hello world\033[0m\0338", out.String())
}

func TestWriteVoiceClear(t *testing.T) {
	t.Parallel()

	var out strings.Builder

	writeVoiceClear(&out)

	require.Equal(t, "\0337\n\033[K\0338", out.String())
}

func TestFakeEventsDescribeTwoUtterances(t *testing.T) {
	t.Parallel()

	events := fakeEvents()

	var finals []string
	speechStarts := 0
	for _, event := range events {
		require.Greater(t, event.delay, time.Duration(0))
		switch event.typ {
		case evSpeechStart:
			speechStarts++
		case evFinal:
			finals = append(finals, event.text)
		}
	}

	require.Equal(t, 2, speechStarts)
	require.Equal(t, []string{"hello world how are you", "this is a test"}, finals)
}

func TestIsTerminalRejectsRegularFiles(t *testing.T) {
	t.Parallel()

	file, err := os.CreateTemp(t.TempDir(), "stdin-*")
	require.NoError(t, err)
	defer file.Close()

	require.False(t, isTerminal(file))
}
