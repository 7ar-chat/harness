module github.com/7ar-chat/harness/app

go 1.26.0

require (
	github.com/7ar-chat/harness v0.8.0
	github.com/7ar-chat/harness/adapters/sandbox v0.3.0
	github.com/7ar-chat/harness/ai v0.2.0
	github.com/7ar-chat/harness/core v0.5.0
	github.com/7ar-chat/harness/hosts/client v0.2.0
	github.com/7ar-chat/harness/hosts/tui v0.3.0
	github.com/7ar-chat/harness/modelinfo v0.2.0
	github.com/alicebob/miniredis/v2 v2.37.0
	github.com/coreos/go-oidc/v3 v3.21.0
	github.com/go-jose/go-jose/v4 v4.1.6
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.9.2
	github.com/redis/go-redis/v9 v9.23.0
	golang.org/x/oauth2 v0.37.0
	golang.org/x/sys v0.48.0
)

require (
	charm.land/bubbles/v2 v2.2.1 // indirect
	charm.land/bubbletea/v2 v2.1.1 // indirect
	charm.land/lipgloss/v2 v2.0.6 // indirect
	github.com/alecthomas/chroma/v2 v2.27.0 // indirect
	github.com/alibaba/OpenSandbox/sdks/sandbox/go v1.2.0 // indirect
	github.com/atotto/clipboard v0.1.4 // indirect
	github.com/aws/aws-sdk-go-v2 v1.47.1 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.20 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.20.6 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.4 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.4 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.4 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.19 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.11.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.4 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.20.4 // indirect
	github.com/aws/aws-sdk-go-v2/service/s3 v1.114.0 // indirect
	github.com/aws/smithy-go v1.28.2 // indirect
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/buger/jsonparser v1.1.2 // indirect
	github.com/caarlos0/env/v11 v11.4.0 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/charmbracelet/colorprofile v0.4.3 // indirect
	github.com/charmbracelet/ultraviolet v0.0.0-20261008173134-6b8d4baf91b4 // indirect
	github.com/charmbracelet/x/ansi v0.11.9 // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/daytonaio/daytona/libs/api-client-go v0.190.0 // indirect
	github.com/daytonaio/daytona/libs/sdk-go v0.190.0 // indirect
	github.com/daytonaio/daytona/libs/toolbox-api-client-go v0.190.0 // indirect
	github.com/dlclark/regexp2/v2 v2.2.1 // indirect
	github.com/eino-contrib/jsonschema v1.0.3 // indirect
	github.com/getsentry/sentry-go v0.50.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/gorilla/websocket v1.5.4-0.20250319132907-e064f32e3674 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.28.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.1 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/mattn/go-runewidth v0.0.30 // indirect
	github.com/modelcontextprotocol/go-sdk v1.8.0 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/wk8/go-ordered-map/v2 v2.1.8 // indirect
	github.com/xo/terminfo v1.0.0 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	github.com/yuin/goldmark v1.8.7 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.45.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp v1.43.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.43.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.43.0 // indirect
	go.opentelemetry.io/otel/metric v1.45.0 // indirect
	go.opentelemetry.io/otel/sdk v1.45.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.45.0 // indirect
	go.opentelemetry.io/otel/trace v1.45.0 // indirect
	go.opentelemetry.io/proto/otlp v1.10.0 // indirect
	go.uber.org/atomic v1.12.0 // indirect
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260401024825-9d38bb4040a9 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260401024825-9d38bb4040a9 // indirect
	google.golang.org/grpc v1.80.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1
)
