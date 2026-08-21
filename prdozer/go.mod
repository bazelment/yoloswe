module github.com/bazelment/yoloswe/prdozer

go 1.25.0

require (
	github.com/bazelment/yoloswe/agent-cli-wrapper v0.0.0
	github.com/bazelment/yoloswe/cliapp v0.0.0
	github.com/bazelment/yoloswe/fleet v0.0.0
	github.com/bazelment/yoloswe/multiagent v0.0.0
	github.com/bazelment/yoloswe/notify v0.0.0-00010101000000-000000000000
	github.com/bazelment/yoloswe/wt v0.0.0
	github.com/spf13/cobra v1.10.2
	github.com/stretchr/testify v1.12.1
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/bazelment/yoloswe/logging v0.0.0 // indirect
	github.com/buger/jsonparser v1.1.2 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/invopop/jsonschema v0.13.0 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	github.com/wk8/go-ordered-map/v2 v2.1.8 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/term v0.43.0 // indirect
)

replace (
	github.com/bazelment/yoloswe/agent-cli-wrapper => ../agent-cli-wrapper
	github.com/bazelment/yoloswe/cliapp => ../cliapp
	github.com/bazelment/yoloswe/fleet => ../fleet
	github.com/bazelment/yoloswe/logging => ../logging
	github.com/bazelment/yoloswe/multiagent => ../multiagent
	github.com/bazelment/yoloswe/notify => ../notify
	github.com/bazelment/yoloswe/wt => ../wt
)
