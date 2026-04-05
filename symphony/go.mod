module github.com/bazelment/yoloswe/symphony

go 1.25.0

require (
	github.com/bazelbuild/rules_go v0.60.0
	github.com/bazelment/yoloswe/logging v0.0.0
	github.com/fsnotify/fsnotify v1.9.0
	github.com/stretchr/testify v1.11.1
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	golang.org/x/sys v0.33.0 // indirect
)

replace github.com/bazelment/yoloswe/logging => ../logging
