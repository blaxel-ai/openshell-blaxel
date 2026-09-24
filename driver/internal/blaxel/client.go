// Package blaxel adapts the official Blaxel Go SDK (github.com/blaxel-ai/sdk-go,
// the SDK behind the `bl` CLI in blaxel-ai/toolkit) to the few operations the
// OpenShell driver needs.
package blaxel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	sdk "github.com/blaxel-ai/sdk-go"
	"github.com/blaxel-ai/sdk-go/option"
)

// ErrNotFound is returned when a sandbox or process does not exist.
var ErrNotFound = errors.New("not found")

type Client struct {
	Workspace string

	sdk   *sdk.Client
	creds sdk.Credentials

	mu        sync.Mutex
	instances map[string]*sdk.SandboxInstance // by sandbox name
}

// NewClient authenticates like `bl`: BL_API_KEY when set, otherwise the
// workspace's `bl login` session (refreshed automatically by the SDK). env is
// "prod" or "dev".
func NewClient(workspace, env string) (*Client, error) {
	if env != "" {
		os.Setenv("BL_ENV", env)
	}
	opts := []option.RequestOption{
		option.WithWorkspace(workspace),
		option.WithHeader("User-Agent", "openshell-driver-blaxel"),
	}
	c := &Client{Workspace: workspace, instances: map[string]*sdk.SandboxInstance{}}
	if key := os.Getenv("BL_API_KEY"); key != "" {
		sdk.InitializeEnvironment(workspace)
		c.creds = sdk.Credentials{APIKey: key}
		client := sdk.NewClient(append(opts, option.WithBaseURL(sdk.GetBaseURL()), option.WithAPIKey(key))...)
		c.sdk = &client
		return c, nil
	}
	client, err := sdk.NewClientFromConfig(workspace, opts...)
	if err != nil {
		return nil, fmt.Errorf("blaxel client for workspace %q (run `bl login %s`): %w", workspace, workspace, err)
	}
	creds, err := sdk.LoadCredentials(workspace)
	if err != nil || !creds.IsValid() {
		return nil, fmt.Errorf("no Blaxel credentials for workspace %q; run `bl login %s` or set BL_API_KEY", workspace, workspace)
	}
	c.sdk, c.creds = client, creds
	return c, nil
}

// Headers returns fresh auth headers for requests the SDK does not make
// itself (the tunnel WebSocket).
func (c *Client) Headers() (http.Header, error) {
	auth, err := c.creds.AuthHeaders(context.Background(), c.Workspace)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	for k, v := range auth {
		h.Set(k, v)
	}
	if bearer := h.Get("X-Blaxel-Authorization"); bearer != "" {
		h.Set("Authorization", bearer)
	}
	h.Set("X-Blaxel-Workspace", c.Workspace)
	return h, nil
}

func notFound(err error) error {
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return err
}

// ---- sandboxes ----

type Port struct {
	Target   int
	Protocol string
}

// SandboxSpec is what the driver creates.
type SandboxSpec struct {
	Name      string
	Labels    map[string]string
	Region    string
	Image     string
	MemoryMiB int
	Ports     []Port
	// ExtraArgs selects the kernel variant, e.g. {"landlock": "enabled"}.
	ExtraArgs map[string]string
}

// Sandbox is the subset of the Blaxel sandbox the driver reads.
type Sandbox struct {
	Name   string
	Labels map[string]string
	URL    string
	Status string
}

func fromSDK(s *sdk.Sandbox) *Sandbox {
	return &Sandbox{Name: s.Metadata.Name, Labels: s.Metadata.Labels, URL: s.Metadata.URL, Status: string(s.Status)}
}

func (c *Client) cache(inst *sdk.SandboxInstance) {
	c.mu.Lock()
	c.instances[inst.Metadata.Name] = inst
	c.mu.Unlock()
}

func (c *Client) CreateSandbox(ctx context.Context, spec SandboxSpec) (*Sandbox, error) {
	ports := make([]sdk.PortParam, 0, len(spec.Ports))
	for _, p := range spec.Ports {
		ports = append(ports, sdk.PortParam{Target: int64(p.Target), Protocol: sdk.PortProtocol(strings.ToUpper(p.Protocol))})
	}
	inst, err := c.sdk.Sandboxes.NewInstance(ctx, sdk.SandboxNewParams{Sandbox: sdk.SandboxParam{
		Metadata: sdk.MetadataParam{Name: spec.Name, Labels: spec.Labels},
		Spec: sdk.SandboxSpecParam{
			Region: sdk.String(spec.Region),
			Runtime: sdk.SandboxRuntimeParam{
				Image:     sdk.String(spec.Image),
				Memory:    sdk.Int(int64(spec.MemoryMiB)),
				Ports:     ports,
				ExtraArgs: spec.ExtraArgs,
			},
		},
	}})
	if err != nil {
		return nil, err
	}
	c.cache(inst)
	return fromSDK(inst.Sandbox), nil
}

func (c *Client) GetSandbox(ctx context.Context, name string) (*Sandbox, error) {
	inst, err := c.sdk.Sandboxes.GetInstance(ctx, name)
	if err != nil {
		return nil, notFound(err)
	}
	c.cache(inst)
	return fromSDK(inst.Sandbox), nil
}

func (c *Client) ListSandboxes(ctx context.Context) ([]Sandbox, error) {
	page, err := c.sdk.Sandboxes.ListInstances(ctx, sdk.SandboxListParams{Limit: sdk.Int(100)})
	var out []Sandbox
	for err == nil && page != nil {
		for _, inst := range page.Data {
			c.cache(inst)
			out = append(out, *fromSDK(inst.Sandbox))
		}
		if !page.HasNextPage() {
			break
		}
		page, err = page.NextPage(ctx)
	}
	return out, err
}

func (c *Client) DeleteSandbox(ctx context.Context, name string) error {
	_, err := c.sdk.Sandboxes.DeleteInstance(ctx, name)
	c.mu.Lock()
	delete(c.instances, name)
	c.mu.Unlock()
	return notFound(err)
}

// instance returns the cached SDK instance (with its sandbox-API client).
func (c *Client) instance(ctx context.Context, name string) (*sdk.SandboxInstance, error) {
	c.mu.Lock()
	inst := c.instances[name]
	c.mu.Unlock()
	if inst != nil {
		return inst, nil
	}
	if _, err := c.GetSandbox(ctx, name); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.instances[name], nil
}

// ---- processes and files (sandbox API) ----

type ProcessRequest struct {
	Name              string
	Command           string
	Env               map[string]string
	WorkingDir        string
	WaitForCompletion bool
	KeepAlive         bool
	// Timeout in seconds. Nil leaves the API default; with KeepAlive, Blaxel
	// kills the process after 600 s unless Timeout is explicitly 0 (Forever).
	Timeout *int
}

// Seconds returns a timeout value for ProcessRequest.
func Seconds(n int) *int { return &n }

// Forever is the timeout for long-lived keepAlive processes.
var Forever = Seconds(0)

type Process struct {
	Name     string
	Status   string // running | completed | failed | killed | stopped
	ExitCode int
	Logs     string
}

// processParam converts a request to the SDK form. An explicit zero timeout
// must survive: param.Opt marks it present, unlike a plain omitempty int.
func processParam(req ProcessRequest) sdk.ProcessRequestParam {
	p := sdk.ProcessRequestParam{Command: req.Command, Env: req.Env}
	if req.Name != "" {
		p.Name = sdk.String(req.Name)
	}
	if req.WorkingDir != "" {
		p.WorkingDir = sdk.String(req.WorkingDir)
	}
	if req.WaitForCompletion {
		p.WaitForCompletion = sdk.Bool(true)
	}
	if req.KeepAlive {
		p.KeepAlive = sdk.Bool(true)
	}
	if req.Timeout != nil {
		p.Timeout = sdk.Int(int64(*req.Timeout))
	}
	return p
}

func (c *Client) Exec(ctx context.Context, sandbox string, req ProcessRequest) (*Process, error) {
	inst, err := c.instance(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	resp, err := inst.Process.New(ctx, processParam(req))
	if err != nil {
		return nil, notFound(err)
	}
	return &Process{Name: resp.Name, Status: string(resp.Status), ExitCode: int(resp.ExitCode), Logs: resp.Logs}, nil
}

// Run executes a shell command to completion and fails on non-zero exit.
func (c *Client) Run(ctx context.Context, sandbox, command string) (string, error) {
	p, err := c.Exec(ctx, sandbox, ProcessRequest{Command: command, WaitForCompletion: true, Timeout: Seconds(55)})
	if err != nil {
		return "", err
	}
	if p.ExitCode != 0 {
		return p.Logs, fmt.Errorf("command exited %d: %s", p.ExitCode, truncate(p.Logs, 500))
	}
	return p.Logs, nil
}

func (c *Client) GetProcess(ctx context.Context, sandbox, name string) (*Process, error) {
	inst, err := c.instance(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	resp, err := inst.Process.Get(ctx, name)
	if err != nil {
		return nil, notFound(err)
	}
	return &Process{Name: resp.Name, Status: string(resp.Status), ExitCode: int(resp.ExitCode), Logs: resp.Logs}, nil
}

func (c *Client) KillProcess(ctx context.Context, sandbox, name string) error {
	inst, err := c.instance(ctx, sandbox)
	if err != nil {
		return err
	}
	_, err = inst.Process.Kill(ctx, name)
	return notFound(err)
}

// WriteFile uploads content to path. The sandbox API currently ignores perm;
// callers must chmod afterwards.
func (c *Client) WriteFile(ctx context.Context, sandbox, path string, content []byte, perm string) error {
	inst, err := c.instance(ctx, sandbox)
	if err != nil {
		return err
	}
	_, err = inst.FS.WriteBinary(ctx, path, content, perm)
	return err
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
