module github.com/7ar-chat/harness

go 1.26.0

require (
	github.com/alibaba/OpenSandbox/sdks/sandbox/go v1.2.0 // indirect
	github.com/alicebob/miniredis/v2 v2.37.0
	github.com/aws/aws-sdk-go-v2 v1.47.1
	github.com/aws/aws-sdk-go-v2/credentials v1.20.6
	github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager v0.4.13
	github.com/aws/aws-sdk-go-v2/service/s3 v1.114.0
	github.com/aws/smithy-go v1.28.2
	github.com/caarlos0/env/v11 v11.4.0
	github.com/charmbracelet/bubbles v1.0.0 // indirect
	github.com/charmbracelet/bubbletea v1.3.10 // indirect
	github.com/charmbracelet/lipgloss v1.1.0 // indirect
	github.com/charmbracelet/x/ansi v0.11.9 // indirect
	github.com/eino-contrib/jsonschema v1.0.3 // indirect
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.8.0
	github.com/modelcontextprotocol/go-sdk v1.8.0
	github.com/muesli/termenv v0.16.0 // indirect
	github.com/redis/go-redis/v9 v9.23.0
	golang.org/x/text v0.42.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/kr/text v0.1.0 // indirect
)

require (
	github.com/coreos/go-oidc/v3 v3.21.0
	github.com/getsentry/sentry-go v0.50.0
	github.com/go-jose/go-jose/v4 v4.1.4
)

require (
	github.com/7ar-chat/harness/adapters/sandbox v0.0.0
	github.com/7ar-chat/harness/ai v0.0.0
	github.com/7ar-chat/harness/core v0.0.0
	github.com/7ar-chat/harness/hosts/client v0.0.0
	github.com/7ar-chat/harness/hosts/tui v0.0.0
	github.com/7ar-chat/harness/modelinfo v0.0.0
	github.com/atotto/clipboard v0.1.4 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.20 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.4 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.4 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.4 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.19 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.11.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.4 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.20.4 // indirect
	github.com/aymanbagabas/go-osc52/v2 v2.0.1 // indirect
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/buger/jsonparser v1.1.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/charmbracelet/colorprofile v0.4.1 // indirect
	github.com/charmbracelet/x/cellbuf v0.0.15 // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/erikgeiser/coninput v0.0.0-20211004153227-1c3628e74d0f // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-localereader v0.0.1 // indirect
	github.com/mattn/go-runewidth v0.0.30 // indirect
	github.com/muesli/ansi v0.0.0-20230316100256-276c6243b2f6 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/wk8/go-ordered-map/v2 v2.1.8 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.uber.org/atomic v1.12.0 // indirect
	golang.org/x/oauth2 v0.37.0
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0
	golang.org/x/time v0.15.0 // indirect
)

replace github.com/7ar-chat/harness/ai => ./ai

replace github.com/7ar-chat/harness/core => ./core

replace github.com/7ar-chat/harness/modelinfo => ./modelinfo

replace github.com/7ar-chat/harness/hosts/client => ./hosts/client

replace github.com/7ar-chat/harness/hosts/tui => ./hosts/tui

replace github.com/7ar-chat/harness/adapters/sandbox => ./adapters/sandbox
