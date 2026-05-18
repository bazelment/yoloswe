package meetingbot

import (
	"testing"

	"github.com/stretchr/testify/require"

	botpkg "github.com/bazelment/yoloswe/bramble/meetingbot"
)

func TestBuildClientFailsClosedToLocal(t *testing.T) {
	// An absent or empty mode must resolve to the local client (the --agent
	// flag default), never the real provider, so a blank value cannot silently
	// trigger network calls or require credentials.
	for _, mode := range []string{"", "  ", "local", "LOCAL", "offline"} {
		c, err := buildClient(mode)
		require.NoError(t, err, "mode=%q", mode)
		require.IsType(t, botpkg.LocalAgentClient{}, c, "mode=%q", mode)
	}

	c, err := buildClient("real")
	require.NoError(t, err)
	require.IsType(t, botpkg.ProviderAgentClient{}, c)

	_, err = buildClient("bogus")
	require.Error(t, err)
}
