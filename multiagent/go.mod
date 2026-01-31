module github.com/bazelment/yoloswe/multiagent

go 1.24.6

require (
	github.com/bazelment/yoloswe/agent-cli-wrapper v0.0.0
	github.com/spf13/cobra v1.10.2
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
)

replace github.com/bazelment/yoloswe/agent-cli-wrapper => ../agent-cli-wrapper
