// Package blaxel is a minimal client for the Blaxel control-plane and sandbox
// APIs, covering what the OpenShell driver needs. Request shapes mirror what
// @blaxel/core sends (traced against API version 2026-04-28).
package blaxel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const apiVersion = "2026-04-28"

// ErrNotFound is returned when a sandbox or process does not exist.
var ErrNotFound = errors.New("not found")

type Client struct {
	Workspace string
	BaseURL   string // e.g. https://api.blaxel.dev/v0
	HTTP      *http.Client

	mu      sync.Mutex
	token   string
	tokenAt time.Time
	apiKey  string
}

// NewClient builds a client for workspace. env is "prod" or "dev". It uses
// BL_API_KEY when set, otherwise the logged-in `bl` CLI session.
func NewClient(workspace, env string) *Client {
	base := "https://api.blaxel.ai/v0"
	if env == "dev" {
		base = "https://api.blaxel.dev/v0"
	}
	return &Client{
		Workspace: workspace,
		BaseURL:   base,
		HTTP:      &http.Client{Timeout: 90 * time.Second},
		apiKey:    os.Getenv("BL_API_KEY"),
	}
}

// Token returns a bearer token, refreshing the CLI session token every few
// minutes (OAuth access tokens are short-lived).
func (c *Client) Token() (string, error) {
	if c.apiKey != "" {
		return c.apiKey, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Since(c.tokenAt) < 5*time.Minute {
		return c.token, nil
	}
	out, err := exec.Command("bl", "token", "-w", c.Workspace).Output()
	if err != nil {
		return "", fmt.Errorf("bl token: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	c.token = strings.TrimSpace(lines[len(lines)-1])
	c.tokenAt = time.Now()
	return c.token, nil
}

// Headers are the auth headers for REST and WebSocket requests.
func (c *Client) Headers() (http.Header, error) {
	tok, err := c.Token()
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("X-Blaxel-Authorization", "Bearer "+tok)
	h.Set("Authorization", "Bearer "+tok)
	h.Set("X-Blaxel-Workspace", c.Workspace)
	h.Set("Blaxel-Version", apiVersion)
	return h, nil
}

func (c *Client) do(ctx context.Context, method, rawURL string, body io.Reader, contentType string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return err
	}
	h, err := c.Headers()
	if err != nil {
		return err
	}
	req.Header = h
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s %s: %w", method, rawURL, ErrNotFound)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, rawURL, resp.StatusCode, truncate(string(data), 300))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: decode: %w", method, rawURL, err)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func jsonBody(v any) (io.Reader, error) {
	b, err := json.Marshal(v)
	return bytes.NewReader(b), err
}

// ---- control plane ----

type Port struct {
	Target   int    `json:"target"`
	Protocol string `json:"protocol,omitempty"`
}

type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Runtime struct {
	Image      string   `json:"image,omitempty"`
	Memory     int      `json:"memory,omitempty"`
	Ports      []Port   `json:"ports,omitempty"`
	Envs       []EnvVar `json:"envs,omitempty"`
	Generation string   `json:"generation,omitempty"`
	// ExtraArgs selects the kernel variant, e.g. {"landlock": "enabled"}.
	ExtraArgs map[string]string `json:"extraArgs,omitempty"`
}

type Metadata struct {
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels,omitempty"`
	URL       string            `json:"url,omitempty"`
	CreatedAt string            `json:"createdAt,omitempty"`
}

type Spec struct {
	Region  string  `json:"region,omitempty"`
	Runtime Runtime `json:"runtime"`
}

type Sandbox struct {
	Metadata Metadata `json:"metadata"`
	Spec     Spec     `json:"spec"`
	Status   string   `json:"status,omitempty"`
}

func (c *Client) CreateSandbox(ctx context.Context, sb Sandbox) (*Sandbox, error) {
	body, err := jsonBody(sb)
	if err != nil {
		return nil, err
	}
	var out Sandbox
	if err := c.do(ctx, http.MethodPost, c.BaseURL+"/sandboxes", body, "application/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetSandbox(ctx context.Context, name string) (*Sandbox, error) {
	var out Sandbox
	if err := c.do(ctx, http.MethodGet, c.BaseURL+"/sandboxes/"+url.PathEscape(name), nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListSandboxes follows cursor pagination and accepts both the bare-array
// and the {data, meta} response shapes.
func (c *Client) ListSandboxes(ctx context.Context) ([]Sandbox, error) {
	var all []Sandbox
	cursor := ""
	for page := 0; page < 100; page++ {
		u := c.BaseURL + "/sandboxes?limit=100"
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		var raw json.RawMessage
		if err := c.do(ctx, http.MethodGet, u, nil, "", &raw); err != nil {
			return nil, err
		}
		var arr []Sandbox
		if json.Unmarshal(raw, &arr) == nil {
			return append(all, arr...), nil
		}
		var wrapped struct {
			Data []Sandbox `json:"data"`
			Meta struct {
				NextCursor string `json:"nextCursor"`
				HasMore    bool   `json:"hasMore"`
			} `json:"meta"`
		}
		if err := json.Unmarshal(raw, &wrapped); err != nil {
			return nil, fmt.Errorf("list sandboxes: decode: %w", err)
		}
		all = append(all, wrapped.Data...)
		if wrapped.Meta.NextCursor == "" || !wrapped.Meta.HasMore {
			return all, nil
		}
		cursor = wrapped.Meta.NextCursor
	}
	return all, nil
}

func (c *Client) DeleteSandbox(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, c.BaseURL+"/sandboxes/"+url.PathEscape(name), nil, "", nil)
}

// ---- sandbox API (per-sandbox URL) ----

type ProcessRequest struct {
	Name              string            `json:"name,omitempty"`
	Command           string            `json:"command"`
	Env               map[string]string `json:"env,omitempty"`
	WorkingDir        string            `json:"workingDir,omitempty"`
	WaitForCompletion bool              `json:"waitForCompletion,omitempty"`
	// Timeout is sent even when zero: with KeepAlive, Blaxel auto-kills the
	// process after 600 s unless timeout is explicitly 0 (infinite).
	Timeout      *int  `json:"timeout,omitempty"`
	KeepAlive    bool  `json:"keepAlive,omitempty"`
	WaitForPorts []int `json:"waitForPorts,omitempty"`
}

type Process struct {
	Name     string `json:"name"`
	PID      string `json:"pid"`
	Status   string `json:"status"` // running | completed | failed | killed | stopped
	ExitCode int    `json:"exitCode"`
	Logs     string `json:"logs"`
}

// Seconds returns a timeout value for ProcessRequest.
func Seconds(n int) *int { return &n }

// Forever is the timeout for long-lived keepAlive processes.
var Forever = Seconds(0)

func (c *Client) Exec(ctx context.Context, sandboxURL string, req ProcessRequest) (*Process, error) {
	body, err := jsonBody(req)
	if err != nil {
		return nil, err
	}
	var out Process
	if err := c.do(ctx, http.MethodPost, sandboxURL+"/process", body, "application/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Run executes a shell command to completion and fails on non-zero exit.
func (c *Client) Run(ctx context.Context, sandboxURL, command string) (string, error) {
	p, err := c.Exec(ctx, sandboxURL, ProcessRequest{Command: command, WaitForCompletion: true, Timeout: Seconds(55)})
	if err != nil {
		return "", err
	}
	if p.ExitCode != 0 {
		return p.Logs, fmt.Errorf("command exited %d: %s", p.ExitCode, truncate(p.Logs, 500))
	}
	return p.Logs, nil
}

func (c *Client) GetProcess(ctx context.Context, sandboxURL, name string) (*Process, error) {
	var out Process
	if err := c.do(ctx, http.MethodGet, sandboxURL+"/process/"+url.PathEscape(name), nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) KillProcess(ctx context.Context, sandboxURL, name string) error {
	return c.do(ctx, http.MethodDelete, sandboxURL+"/process/"+url.PathEscape(name)+"/kill", nil, "", nil)
}

// WriteFile uploads content to path. The sandbox API currently ignores perm;
// callers must chmod afterwards.
func (c *Client) WriteFile(ctx context.Context, sandboxURL, path string, content []byte, perm string) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "upload.bin")
	if err != nil {
		return err
	}
	fw.Write(content)
	mw.WriteField("permissions", perm)
	mw.WriteField("path", path)
	mw.Close()
	return c.do(ctx, http.MethodPut, sandboxURL+"/filesystem/"+path, &buf, mw.FormDataContentType(), nil)
}
