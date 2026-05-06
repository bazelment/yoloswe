module github.com/bazelment/yoloswe/symphony

go 1.25.0

require (
	github.com/bazelbuild/rules_go v0.60.0
	github.com/bazelment/yoloswe/cliapp v0.0.0
	github.com/fsnotify/fsnotify v1.10.1
	github.com/spf13/cobra v1.10.2
	github.com/stretchr/testify v1.11.1
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/bazelment/yoloswe/agent-cli-wrapper v0.0.0 // indirect
	github.com/bazelment/yoloswe/logging v0.0.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	golang.org/x/sys v0.43.0 // indirect
	golang.org/x/term v0.41.0 // indirect
)

replace github.com/bazelment/yoloswe/agent-cli-wrapper => ../agent-cli-wrapper

replace github.com/bazelment/yoloswe/cliapp => ../cliapp

replace github.com/bazelment/yoloswe/logging => ../logging
