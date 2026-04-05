module github.com/bazelment/yoloswe/voice

go 1.25.0

require (
	github.com/bazelment/yoloswe/logging v0.0.0
	github.com/deepgram/deepgram-go-sdk/v3 v3.5.0
	github.com/gorilla/websocket v1.5.3
	github.com/stretchr/testify v1.11.1
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/dvonthenen/websocket v1.5.1-dyv.2 // indirect
	github.com/fatih/color v1.15.0 // indirect
	github.com/go-logr/logr v1.3.0 // indirect
	github.com/gorilla/schema v1.3.0 // indirect
	github.com/hokaccha/go-prettyjson v0.0.0-20211117102719-0474bc63780f // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.17 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	golang.org/x/sys v0.25.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	k8s.io/klog/v2 v2.110.1 // indirect
)

replace github.com/bazelment/yoloswe/logging => ../logging
