module github.com/7ar-chat/harness/adapters/sandbox

go 1.26.0

require (
	github.com/7ar-chat/harness/core v0.0.0
	github.com/alibaba/OpenSandbox/sdks/sandbox/go v1.2.0
	github.com/google/uuid v1.6.0
)

require golang.org/x/sync v0.7.0 // indirect

replace github.com/7ar-chat/harness/core => ../../core
