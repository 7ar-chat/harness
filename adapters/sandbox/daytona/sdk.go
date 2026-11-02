package daytona

import (
	"context"
	"fmt"
	"time"

	apiclient "github.com/daytonaio/daytona/libs/api-client-go"
	sdk "github.com/daytonaio/daytona/libs/sdk-go/pkg/daytona"
	sdkerrors "github.com/daytonaio/daytona/libs/sdk-go/pkg/errors"
	"github.com/daytonaio/daytona/libs/sdk-go/pkg/options"
	"github.com/daytonaio/daytona/libs/sdk-go/pkg/types"
)

// This file is the whole surface of the Daytona SDK the adapter uses. Everything else talks to the small
// interfaces below, which the tests fake.

// createParams are the creation parameters the adapter decides on.
type createParams struct {
	Snapshot         string
	Labels           map[string]string
	AutoStopMinutes  int
	AutoDeleteMin    *int
	NetworkBlockAll  bool
	NetworkAllowList string // comma separated CIDRs
	DomainAllowList  string // comma separated domains
}

// boxInfo is the part of a sandbox's state the adapter looks at.
type boxInfo struct {
	State       string // Daytona sandbox state ("started", "stopped", "destroyed", ...)
	Recoverable bool   // meaningful in the "error" state
}

type fileEntry struct {
	Name  string
	Size  int64
	IsDir bool
	MTime time.Time
}

// api is the account-level surface.
type api interface {
	Create(ctx context.Context, p createParams) (box, error)
	Get(ctx context.Context, id string) (box, error)
	SetAutoStop(ctx context.Context, id string, minutes int) error
}

// box is one sandbox.
type box interface {
	ID() string
	Info() boxInfo
	Refresh(ctx context.Context) error
	Start(ctx context.Context) error
	Delete(ctx context.Context) error

	// Run executes a command to completion (combined output, no streaming).
	Run(ctx context.Context, command string, cwd string, env map[string]string) (exit int, output string, err error)

	// Sessions give streaming and separate stdout/stderr.
	CreateSession(ctx context.Context, session string) error
	DeleteSession(ctx context.Context, session string) error
	StartCommand(ctx context.Context, session, command string) (cmdID string, err error)
	CommandExit(ctx context.Context, session, cmdID string) (exit int, done bool, err error)
	StreamLogs(ctx context.Context, session, cmdID string, stdout, stderr chan<- string) error
	CommandLogs(ctx context.Context, session, cmdID string) (stdout, stderr string, err error)

	ListFiles(ctx context.Context, dir string) ([]fileEntry, error)
	FileInfo(ctx context.Context, path string) (fileEntry, error)
	Download(ctx context.Context, path string) ([]byte, error)
	Upload(ctx context.Context, path string, data []byte) error
}

// sdkAPI implements api on the official SDK.
type sdkAPI struct {
	client *sdk.Client
	api    *apiclient.APIClient // for the calls the SDK does not wrap (auto-stop)
	token  string
}

func newSDKAPI(o Options) (*sdkAPI, error) {
	c, err := sdk.NewClientWithConfig(&types.DaytonaConfig{APIKey: o.APIKey, APIUrl: o.URL, Target: o.Target})
	if err != nil {
		return nil, err
	}
	u := o.URL
	if u == "" {
		u = "https://app.daytona.io/api"
	}
	cfg := apiclient.NewConfiguration()
	cfg.Servers = apiclient.ServerConfigurations{{URL: u}}
	return &sdkAPI{client: c, api: apiclient.NewAPIClient(cfg), token: o.APIKey}, nil
}

func (a *sdkAPI) Create(ctx context.Context, p createParams) (box, error) {
	base := types.SandboxBaseParams{
		Labels:             p.Labels,
		AutoStopInterval:   &p.AutoStopMinutes,
		AutoDeleteInterval: p.AutoDeleteMin,
		NetworkBlockAll:    p.NetworkBlockAll,
	}
	if p.NetworkAllowList != "" {
		base.NetworkAllowList = &p.NetworkAllowList
	}
	if p.DomainAllowList != "" {
		base.DomainAllowList = &p.DomainAllowList
	}
	s, err := a.client.Create(ctx, types.SnapshotParams{SandboxBaseParams: base, Snapshot: p.Snapshot}, options.WithWaitForStart(true))
	if err != nil {
		return nil, err
	}
	return &sdkBox{s: s}, nil
}

func (a *sdkAPI) Get(ctx context.Context, id string) (box, error) {
	s, err := a.client.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &sdkBox{s: s}, nil
}

func (a *sdkAPI) SetAutoStop(ctx context.Context, id string, minutes int) error {
	ctx = context.WithValue(ctx, apiclient.ContextAccessToken, a.token)
	_, resp, err := a.api.SandboxAPI.SetAutostopInterval(ctx, id, float32(minutes)).Execute()
	if err != nil {
		return sdkerrors.ConvertAPIError(err, resp)
	}
	return nil
}

type sdkBox struct{ s *sdk.Sandbox }

func (b *sdkBox) ID() string { return b.s.ID }

func (b *sdkBox) Info() boxInfo {
	i := boxInfo{State: string(b.s.State)}
	if b.s.Recoverable != nil {
		i.Recoverable = *b.s.Recoverable
	}
	return i
}

func (b *sdkBox) Refresh(ctx context.Context) error { return b.s.RefreshData(ctx) }
func (b *sdkBox) Start(ctx context.Context) error   { return b.s.Start(ctx) }
func (b *sdkBox) Delete(ctx context.Context) error  { return b.s.Delete(ctx) }

func (b *sdkBox) Run(ctx context.Context, command, cwd string, env map[string]string) (int, string, error) {
	var opts []func(*options.ExecuteCommand)
	if cwd != "" {
		opts = append(opts, options.WithCwd(cwd))
	}
	if len(env) > 0 {
		opts = append(opts, options.WithCommandEnv(env))
	}
	r, err := b.s.Process.ExecuteCommand(ctx, command, opts...)
	if err != nil {
		return 0, "", err
	}
	return r.ExitCode, r.Result, nil
}

func (b *sdkBox) CreateSession(ctx context.Context, session string) error {
	return b.s.Process.CreateSession(ctx, session)
}

func (b *sdkBox) DeleteSession(ctx context.Context, session string) error {
	return b.s.Process.DeleteSession(ctx, session)
}

func (b *sdkBox) StartCommand(ctx context.Context, session, command string) (string, error) {
	m, err := b.s.Process.ExecuteSessionCommand(ctx, session, command, true, true)
	if err != nil {
		return "", err
	}
	id, _ := m["id"].(string)
	if id == "" {
		return "", fmt.Errorf("daytona returned no command id")
	}
	return id, nil
}

func (b *sdkBox) CommandExit(ctx context.Context, session, cmdID string) (int, bool, error) {
	m, err := b.s.Process.GetSessionCommand(ctx, session, cmdID)
	if err != nil {
		return 0, false, err
	}
	switch v := m["exitCode"].(type) {
	case int:
		return v, true, nil
	case int32:
		return int(v), true, nil
	case int64:
		return int(v), true, nil
	case float32:
		return int(v), true, nil
	case float64:
		return int(v), true, nil
	}
	return 0, false, nil
}

func (b *sdkBox) StreamLogs(ctx context.Context, session, cmdID string, stdout, stderr chan<- string) error {
	return b.s.Process.GetSessionCommandLogsStream(ctx, session, cmdID, stdout, stderr)
}

func (b *sdkBox) CommandLogs(ctx context.Context, session, cmdID string) (string, string, error) {
	r, err := b.s.Process.GetSessionCommandLogs(ctx, session, cmdID)
	if err != nil {
		return "", "", err
	}
	if r.Stdout == "" && r.Stderr == "" {
		return r.Output, "", nil
	}
	return r.Stdout, r.Stderr, nil
}

func (b *sdkBox) ListFiles(ctx context.Context, dir string) ([]fileEntry, error) {
	fs, err := b.s.FileSystem.ListFiles(ctx, dir)
	if err != nil {
		return nil, err
	}
	out := make([]fileEntry, len(fs))
	for i, f := range fs {
		out[i] = fileEntry{Name: f.Name, Size: f.Size, IsDir: f.IsDirectory, MTime: f.ModifiedTime}
	}
	return out, nil
}

func (b *sdkBox) FileInfo(ctx context.Context, path string) (fileEntry, error) {
	f, err := b.s.FileSystem.GetFileInfo(ctx, path)
	if err != nil {
		return fileEntry{}, err
	}
	return fileEntry{Name: f.Name, Size: f.Size, IsDir: f.IsDirectory, MTime: f.ModifiedTime}, nil
}

func (b *sdkBox) Download(ctx context.Context, path string) ([]byte, error) {
	return b.s.FileSystem.DownloadFile(ctx, path, nil)
}

func (b *sdkBox) Upload(ctx context.Context, path string, data []byte) error {
	return b.s.FileSystem.UploadFile(ctx, data, path)
}
