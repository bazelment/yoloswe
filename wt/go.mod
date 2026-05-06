module github.com/bazelment/yoloswe/wt

go 1.25.0

require (
	github.com/bazelment/yoloswe/agent-cli-wrapper v0.0.0
	github.com/bazelment/yoloswe/bramble v0.0.0
	github.com/bazelment/yoloswe/cliapp v0.0.0
	github.com/bazelment/yoloswe/multiagent v0.0.0
	github.com/spf13/cobra v1.10.2
	github.com/stretchr/testify v1.11.1
	golang.org/x/sync v0.19.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/bazelment/yoloswe/logging v0.0.0 // indirect
	github.com/buger/jsonparser v1.1.1 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/invopop/jsonschema v0.13.0 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	github.com/wk8/go-ordered-map/v2 v2.1.8 // indirect
	golang.org/x/sys v0.43.0 // indirect
	golang.org/x/term v0.41.0 // indirect
)

replace github.com/bazelment/yoloswe/cliapp => ../cliapp

replace github.com/bazelment/yoloswe/logging => ../logging

replace github.com/bazelment/yoloswe/agent-cli-wrapper => ../agent-cli-wrapper

replace github.com/bazelment/yoloswe/bramble => ../bramble

replace github.com/bazelment/yoloswe/multiagent => ../multiagent
