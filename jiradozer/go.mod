module github.com/bazelment/yoloswe/jiradozer

go 1.25.0

require (
	charm.land/bubbletea/v2 v2.0.2
	charm.land/lipgloss/v2 v2.0.2
	github.com/bazelment/yoloswe/logging v0.0.0
	github.com/bazelment/yoloswe/multiagent v0.0.0
	github.com/bazelment/yoloswe/wt v0.0.0
	github.com/spf13/cobra v1.10.2
	github.com/stretchr/testify v1.11.1
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/bazelment/yoloswe/agent-cli-wrapper v0.0.0 // indirect
	github.com/buger/jsonparser v1.1.1 // indirect
	github.com/charmbracelet/colorprofile v0.4.2 // indirect
	github.com/charmbracelet/ultraviolet v0.0.0-20260205113103-524a6607adb8 // indirect
	github.com/charmbracelet/x/ansi v0.11.6 // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/invopop/jsonschema v0.13.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.3.0 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/mattn/go-runewidth v0.0.19 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	github.com/wk8/go-ordered-map/v2 v2.1.8 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
)

replace (
	github.com/bazelment/yoloswe/agent-cli-wrapper => ../agent-cli-wrapper
	github.com/bazelment/yoloswe/logging => ../logging
	github.com/bazelment/yoloswe/multiagent => ../multiagent
	github.com/bazelment/yoloswe/wt => ../wt
)
