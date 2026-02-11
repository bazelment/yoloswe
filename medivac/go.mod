module github.com/bazelment/yoloswe/medivac

go 1.24.6

require (
	github.com/bazelment/yoloswe/agent-cli-wrapper v0.0.0
	github.com/bazelment/yoloswe/multiagent v0.0.0
	github.com/bazelment/yoloswe/wt v0.0.0
	github.com/spf13/cobra v1.10.2
)

require (
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/buger/jsonparser v1.1.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/invopop/jsonschema v0.13.0 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	github.com/wk8/go-ordered-map/v2 v2.1.8 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	github.com/bazelment/yoloswe/agent-cli-wrapper => ../agent-cli-wrapper
	github.com/bazelment/yoloswe/multiagent => ../multiagent
	github.com/bazelment/yoloswe/wt => ../wt
)
