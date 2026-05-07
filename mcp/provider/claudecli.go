// Package provider — ClaudeCLIClient runs a local `claude` (Claude Code CLI)
// binary in non-interactive print mode and uses its stdout as the LLM reply.
//
// Auth uses whatever `claude` is logged in with (OAuth subscription, ANTHROPIC_API_KEY,
// etc.) — this client never speaks HTTP itself, so no NOFX-side API key is required.
//
// Scope:
//   - Implements only CallWithMessages (the path used by trader/strategy decision).
//   - CallWithRequest collapses messages to a single user prompt and reuses the same path.
//   - Streaming and tool-call paths return errors (not used by the trading loop).
package provider

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"nofx/mcp"
)

const (
	defaultClaudeCLIBin  = "claude"
	defaultClaudeCLIWait = 120 * time.Second
)

// disabledTools is the set we explicitly forbid claude CLI from invoking during a
// decision call. The trading prompt asks for a single XML/JSON reply; any tool
// call would derail the response and burn time.
var disabledTools = strings.Join([]string{
	"Read", "Edit", "Write", "Bash", "Grep", "Glob",
	"Agent", "WebFetch", "WebSearch", "TodoWrite", "Task",
}, " ")

func init() {
	mcp.RegisterProvider(mcp.ProviderClaudeCLI, func(opts ...mcp.ClientOption) mcp.AIClient {
		return NewClaudeCLIClientWithOptions(opts...)
	})
}

// ClaudeCLIClient invokes the `claude` binary for each call.
type ClaudeCLIClient struct {
	*mcp.Client

	binPath string        // path to claude binary
	timeout time.Duration // hard timeout per call
	workDir string        // cwd for the invocation (kept neutral so CLAUDE.md isn't injected)
}

// BaseClient satisfies mcp.ClientEmbedder.
func (c *ClaudeCLIClient) BaseClient() *mcp.Client { return c.Client }

// NewClaudeCLIClient returns a CLI-backed AIClient using defaults.
func NewClaudeCLIClient() mcp.AIClient {
	return NewClaudeCLIClientWithOptions()
}

// NewClaudeCLIClientWithOptions returns a CLI-backed AIClient.
//
// The mcp.ClientOption values are accepted for interface symmetry; APIKey/BaseURL
// are unused (the CLI binary handles auth itself). WithModel passes through to
// `--model`. WithTimeout caps each invocation.
func NewClaudeCLIClientWithOptions(opts ...mcp.ClientOption) mcp.AIClient {
	baseClient := mcp.NewClient(append([]mcp.ClientOption{
		mcp.WithProvider(mcp.ProviderClaudeCLI),
	}, opts...)...).(*mcp.Client)

	bin := os.Getenv("NOFX_CLAUDE_CLI_BIN")
	if bin == "" {
		bin = defaultClaudeCLIBin
	}
	workDir := os.Getenv("NOFX_CLAUDE_CLI_CWD")
	if workDir == "" {
		workDir = os.TempDir()
	}

	c := &ClaudeCLIClient{
		Client:  baseClient,
		binPath: bin,
		timeout: defaultClaudeCLIWait,
		workDir: workDir,
	}
	if baseClient.HTTPClient != nil && baseClient.HTTPClient.Timeout > 0 {
		c.timeout = baseClient.HTTPClient.Timeout
	}
	return c
}

// SetAPIKey is a no-op for the CLI client. customModel maps to `--model`.
func (c *ClaudeCLIClient) SetAPIKey(_ /*apiKey*/, _ /*customURL*/, customModel string) {
	if customModel != "" {
		c.Model = customModel
	}
}

// SetTimeout overrides the per-call timeout.
func (c *ClaudeCLIClient) SetTimeout(timeout time.Duration) {
	if timeout > 0 {
		c.timeout = timeout
	}
}

// CallWithMessages runs the claude CLI once with the given prompts and returns stdout.
func (c *ClaudeCLIClient) CallWithMessages(systemPrompt, userPrompt string) (string, error) {
	if strings.TrimSpace(userPrompt) == "" {
		return "", fmt.Errorf("claudecli: empty user prompt")
	}

	sysFile, cleanup, err := writeTempPrompt(systemPrompt)
	if err != nil {
		return "", fmt.Errorf("claudecli: write system prompt: %w", err)
	}
	defer cleanup()

	args := []string{
		"-p",
		"--output-format", "text",
		"--append-system-prompt-file", sysFile,
		"--disallowedTools", disabledTools,
	}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.binPath, args...)
	cmd.Dir = c.workDir
	cmd.Stdin = strings.NewReader(userPrompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	if err := cmd.Run(); err != nil {
		// Claude CLI sometimes prints fatal errors to stdout (the JSON output
		// stream) rather than stderr, especially for argv-validation issues.
		// Surface both so debugging doesn't require running the binary by hand.
		return "", fmt.Errorf("claudecli: run failed after %s: %w (stderr: %s | stdout: %s)",
			time.Since(start).Truncate(time.Millisecond), err,
			truncate(stderr.String(), 400),
			truncate(stdout.String(), 400))
	}

	out := strings.TrimSpace(stdout.String())
	if out == "" {
		return "", fmt.Errorf("claudecli: empty stdout (stderr: %s)", truncate(stderr.String(), 400))
	}
	return out, nil
}

// CallWithRequest collapses messages back to (system, user) and reuses CallWithMessages.
// Tools/streaming aren't supported.
func (c *ClaudeCLIClient) CallWithRequest(req *mcp.Request) (string, error) {
	if req == nil {
		return "", fmt.Errorf("claudecli: nil request")
	}
	if len(req.Tools) > 0 {
		return "", fmt.Errorf("claudecli: tool-call requests not supported")
	}
	var sys, user strings.Builder
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			if sys.Len() > 0 {
				sys.WriteString("\n\n")
			}
			sys.WriteString(m.Content)
		default:
			if user.Len() > 0 {
				user.WriteString("\n\n")
			}
			user.WriteString(m.Content)
		}
	}
	return c.CallWithMessages(sys.String(), user.String())
}

// CallWithRequestStream is not implemented; falls back to non-streaming and emits one chunk.
func (c *ClaudeCLIClient) CallWithRequestStream(req *mcp.Request, onChunk func(string)) (string, error) {
	out, err := c.CallWithRequest(req)
	if err != nil {
		return "", err
	}
	if onChunk != nil {
		onChunk(out)
	}
	return out, nil
}

// CallWithRequestFull returns text only; tool calls are unsupported.
func (c *ClaudeCLIClient) CallWithRequestFull(req *mcp.Request) (*mcp.LLMResponse, error) {
	out, err := c.CallWithRequest(req)
	if err != nil {
		return nil, err
	}
	return &mcp.LLMResponse{Content: out}, nil
}

// writeTempPrompt persists a prompt to a tmp file and returns a cleanup fn.
func writeTempPrompt(content string) (string, func(), error) {
	f, err := os.CreateTemp("", "nofx-claudecli-*.txt")
	if err != nil {
		return "", func() {}, err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", func() {}, err
	}
	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
