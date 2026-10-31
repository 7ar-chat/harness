module github.com/GhaziBenDahmane/harness/adapters/sandbox

go 1.26.0

require (
	github.com/GhaziBenDahmane/harness/core v0.0.0
	github.com/alibaba/OpenSandbox/sdks/sandbox/go v1.2.0
	github.com/google/uuid v1.6.0
)

require golang.org/x/sync v0.7.0 // indirect

replace github.com/GhaziBenDahmane/harness/core => ../../core
