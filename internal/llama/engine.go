// Package llama starts and owns a local llama.cpp server and its inference client.
package llama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/matthewalexandern/AI/internal/memory"
)

const (
	maxResponseBytes   = 4 << 20
	maxStartupLogBytes = 16 << 10
	shutdownGrace      = 2 * time.Second
)

// Options describes the already-installed server and the selected GGUF model.
// Backend identifies how the executable was built; CPU always disables offload.
type Options struct {
	Executable     string
	Model          string
	Backend        string
	ContextSize    int
	GpuLayers      int
	Threads        int
	Port           int
	StartupTimeout time.Duration
	Log            io.Writer
}

// Engine owns exactly one child process. Close may be called repeatedly.
type Engine struct {
	cmd         *exec.Cmd
	baseURL     string
	client      *http.Client
	ctx         context.Context
	cancel      context.CancelFunc
	processDone chan struct{}
	gate        chan struct{}
	logs        *startupLog
	closeOnce   sync.Once
	closeErr    error
	waitMu      sync.Mutex
	waitErr     error
}

// Start launches llama.cpp without a shell and waits for its health endpoint.
// Cancellation of ctx closes the child, including after startup succeeds.
func Start(ctx context.Context, opts Options) (*Engine, error) {
	if ctx == nil {
		return nil, errors.New("llama.cpp: context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	opts, err := validateOptions(opts)
	if err != nil {
		return nil, err
	}
	environment, err := childEnvironment(opts.Executable, os.Environ(), runtime.GOOS)
	if err != nil {
		return nil, err
	}
	port, err := availablePort(opts.Port)
	if err != nil {
		return nil, fmt.Errorf("llama.cpp loopback port: %w", err)
	}
	opts.Port = port
	lifeCtx, cancel := context.WithCancel(ctx)
	e := &Engine{
		cmd:     exec.Command(opts.Executable, serverArgs(opts)...),
		baseURL: "http://127.0.0.1:" + strconv.Itoa(port),
		client: &http.Client{
			Transport:     &http.Transport{Proxy: nil},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		ctx:         lifeCtx,
		cancel:      cancel,
		processDone: make(chan struct{}),
		gate:        make(chan struct{}, 1),
		logs:        &startupLog{writer: opts.Log},
	}
	e.cmd.Env = environment
	e.cmd.Stdout = e.logs
	e.cmd.Stderr = e.logs
	if err := e.cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start llama.cpp: %w", err)
	}
	// This goroutine is the only owner of Wait, including all startup failures.
	go func() {
		err := e.cmd.Wait()
		e.waitMu.Lock()
		e.waitErr = err
		e.waitMu.Unlock()
		close(e.processDone)
	}()
	go func() {
		select {
		case <-lifeCtx.Done():
		case <-e.processDone:
		}
		_ = e.Close()
	}()
	if err := e.waitReady(opts.StartupTimeout); err != nil {
		_ = e.Close()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, e.startupError(err)
	}
	if err := ctx.Err(); err != nil {
		_ = e.Close()
		return nil, e.startupError(err)
	}
	return e, nil
}

func validateOptions(opts Options) (Options, error) {
	for label, target := range map[string]*string{"executable": &opts.Executable, "model": &opts.Model} {
		if strings.TrimSpace(*target) == "" {
			return opts, fmt.Errorf("llama.cpp %s path is required", label)
		}
		absolute, err := filepath.Abs(*target)
		if err != nil {
			return opts, fmt.Errorf("llama.cpp %s path: %w", label, err)
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return opts, fmt.Errorf("llama.cpp %s: %w", label, err)
		}
		if !info.Mode().IsRegular() {
			return opts, fmt.Errorf("llama.cpp %s must be a regular file", label)
		}
		if label == "executable" && runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0 {
			return opts, errors.New("llama.cpp executable has no execute permission")
		}
		*target = absolute
	}
	opts.Backend = strings.ToLower(strings.TrimSpace(opts.Backend))
	if opts.Backend == "" {
		opts.Backend = "auto"
	}
	switch opts.Backend {
	case "auto", "cpu", "cuda", "metal", "vulkan":
	default:
		return opts, fmt.Errorf("unsupported llama.cpp backend %q", opts.Backend)
	}
	if opts.Port < 0 || opts.Port > 65535 {
		return opts, errors.New("llama.cpp port must be between 0 and 65535")
	}
	if opts.ContextSize < 0 || opts.Threads < 0 || opts.GpuLayers < -1 {
		return opts, errors.New("llama.cpp context and threads must be nonnegative; GPU layers must be -1 or nonnegative")
	}
	if opts.ContextSize == 0 {
		opts.ContextSize = 4096
	}
	if opts.Threads == 0 {
		opts.Threads = runtime.NumCPU()
	}
	if opts.Backend == "cpu" {
		opts.GpuLayers = 0
	}
	if opts.StartupTimeout < 0 {
		return opts, errors.New("llama.cpp startup timeout must be nonnegative")
	}
	if opts.StartupTimeout == 0 {
		opts.StartupTimeout = 2 * time.Minute
	}
	return opts, nil
}

func serverArgs(opts Options) []string {
	return []string{
		"--host", "127.0.0.1", "--port", strconv.Itoa(opts.Port),
		"--model", opts.Model, "--alias", "local",
		"--ctx-size", strconv.Itoa(opts.ContextSize),
		"--threads", strconv.Itoa(opts.Threads),
		"--n-gpu-layers", strconv.Itoa(opts.GpuLayers),
		"--parallel", "1",
		"--jinja", "--reasoning-format", "deepseek",
		// v0.6 maps llama/GGML INFO to trace verbosity 4. Model buffer and
		// offload diagnostics are required to verify the actual backend.
		"--log-verbosity", "4",
	}
}

func availablePort(requested int) (int, error) {
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(requested)))
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

func (e *Engine) waitReady(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(e.ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var healthErr error
	for {
		select {
		case <-e.processDone:
			return e.exitError()
		case <-ctx.Done():
			if healthErr != nil {
				return fmt.Errorf("waiting for healthy server: %w (last health check: %v)", ctx.Err(), healthErr)
			}
			return fmt.Errorf("waiting for healthy server: %w", ctx.Err())
		default:
		}
		healthErr = e.checkHealth(ctx)
		if healthErr == nil {
			select {
			case <-e.processDone:
				return e.exitError()
			default:
				return nil
			}
		}
		select {
		case <-e.processDone:
			return e.exitError()
		case <-ctx.Done():
			return fmt.Errorf("waiting for healthy server: %w (last health check: %v)", ctx.Err(), healthErr)
		case <-ticker.C:
		}
	}
}

func (e *Engine) checkHealth(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, e.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	response, err := e.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	var health struct {
		Status string `json:"status"`
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil {
		return fmt.Errorf("read health response: %w", err)
	}
	if len(encoded) > 4096 {
		return errors.New("health response exceeds 4 KiB limit")
	}
	if err := json.Unmarshal(encoded, &health); err != nil {
		return fmt.Errorf("invalid health response: %w", err)
	}
	if health.Status != "ok" {
		return fmt.Errorf("server health is %q", health.Status)
	}
	return nil
}

// BaseURL returns the server's loopback URL.
func (e *Engine) BaseURL() string { return e.baseURL }

// Complete sends one bounded, non-streaming request. Requests are serialized so
// the owned server's single inference slot is not overwhelmed.
func (e *Engine) Complete(ctx context.Context, messages []memory.Message, temperature float64) (string, error) {
	return e.complete(ctx, messages, temperature, nil)
}

type responseFormat struct {
	Type       string `json:"type"`
	JSONSchema struct {
		Name   string          `json:"name"`
		Strict bool            `json:"strict"`
		Schema json.RawMessage `json:"schema"`
	} `json:"json_schema"`
}

// CompleteJSON constrains generation with llama.cpp's JSON Schema grammar.
// Callers still validate the returned data: a grammar shapes output, but does
// not establish the truth or quality of the generated assessment.
func (e *Engine) CompleteJSON(ctx context.Context, messages []memory.Message, temperature float64, schema json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(schema)
	if len(schema) > 64<<10 || len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return "", errors.New("llama.cpp: JSON schema must be one JSON object of at most 64 KiB")
	}
	format := &responseFormat{Type: "json_schema"}
	format.JSONSchema.Name = "cognitive_assessment"
	format.JSONSchema.Strict = true
	format.JSONSchema.Schema = schema
	return e.complete(ctx, messages, temperature, format)
}

func (e *Engine) complete(ctx context.Context, messages []memory.Message, temperature float64, format *responseFormat) (string, error) {
	if ctx == nil {
		return "", errors.New("llama.cpp: context is required")
	}
	if len(messages) == 0 {
		return "", errors.New("llama.cpp: at least one message is required")
	}
	if math.IsNaN(temperature) || math.IsInf(temperature, 0) || temperature < 0 || temperature > 2 {
		return "", errors.New("llama.cpp: temperature must be between 0 and 2")
	}
	type chatMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	chat := make([]chatMessage, len(messages))
	for i, message := range messages {
		switch message.Role {
		case "system", "user", "assistant":
		default:
			return "", fmt.Errorf("llama.cpp: unsupported message role %q", message.Role)
		}
		chat[i] = chatMessage{Role: message.Role, Content: message.Content}
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-e.ctx.Done():
		return "", errors.New("llama.cpp engine is closed")
	case <-e.processDone:
		return "", e.exitError()
	case e.gate <- struct{}{}:
	}
	defer func() { <-e.gate }()
	// A ready gate can win the select at the same time as cancellation. Check
	// again before building a request so a closed engine never accepts work.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := e.ctx.Err(); err != nil {
		return "", errors.New("llama.cpp engine is closed")
	}
	requestCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(e.ctx, cancel)
	defer stop()
	defer cancel()
	payload := struct {
		Model           string          `json:"model"`
		Messages        []chatMessage   `json:"messages"`
		Temperature     float64         `json:"temperature"`
		Stream          bool            `json:"stream"`
		MaxTokens       int             `json:"max_tokens"`
		Template        map[string]bool `json:"chat_template_kwargs"`
		ResponseFormat  *responseFormat `json:"response_format,omitempty"`
		ReasoningEffort string          `json:"reasoning_effort"`
	}{Model: "local", Messages: chat, Temperature: temperature, MaxTokens: 1024, Template: map[string]bool{"enable_thinking": false}, ResponseFormat: format, ReasoningEffort: "low"}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode llama.cpp request: %w", err)
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, e.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := e.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("llama.cpp inference: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llama.cpp inference returned HTTP %d", response.StatusCode)
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("read llama.cpp response: %w", err)
	}
	if len(encoded) > maxResponseBytes {
		return "", errors.New("llama.cpp response exceeds 4 MiB limit")
	}
	var result struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return "", fmt.Errorf("decode llama.cpp response: %w", err)
	}
	if len(result.Choices) > 0 && result.Choices[0].FinishReason == "length" {
		return "", errors.New("llama.cpp returned a truncated answer at its output token limit")
	}
	if len(result.Choices) > 0 && result.Choices[0].FinishReason != "" && result.Choices[0].FinishReason != "stop" {
		return "", fmt.Errorf("llama.cpp did not complete a text answer (finish_reason %q)", result.Choices[0].FinishReason)
	}
	if len(result.Choices) == 0 || strings.TrimSpace(result.Choices[0].Message.Content) == "" {
		return "", errors.New("llama.cpp returned an empty answer")
	}
	// The pinned server's GPT-OSS parser separates analysis into the distinct
	// reasoning_content field, which this adapter never decodes or returns.
	// An exact raw Harmony analysis header means parsing failed; do not leak
	// that channel or try to heuristically strip ordinary prose or think tags.
	if strings.Contains(result.Choices[0].Message.Content, "<|channel|>analysis<|message|>") {
		return "", errors.New("llama.cpp returned an unparsed Harmony analysis channel")
	}
	if err := requestCtx.Err(); err != nil {
		return "", err
	}
	return result.Choices[0].Message.Content, nil
}

// Close cancels in-flight requests, allows a short graceful shutdown on Unix,
// then kills the child if needed. The single Wait goroutine reaps the process.
func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		e.cancel()
		e.client.CloseIdleConnections()
		if e.cmd == nil || e.cmd.Process == nil {
			return
		}
		select {
		case <-e.processDone:
			return
		default:
		}
		if runtime.GOOS != "windows" {
			if err := e.cmd.Process.Signal(os.Interrupt); err == nil {
				timer := time.NewTimer(shutdownGrace)
				select {
				case <-e.processDone:
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}
		if err := e.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			e.closeErr = fmt.Errorf("kill llama.cpp: %w", err)
			return
		}
		<-e.processDone
	})
	return e.closeErr
}

func (e *Engine) exitError() error {
	e.waitMu.Lock()
	defer e.waitMu.Unlock()
	if e.waitErr != nil {
		return fmt.Errorf("llama.cpp process exited: %w", e.waitErr)
	}
	return errors.New("llama.cpp process exited")
}

func (e *Engine) startupError(err error) error {
	if output := strings.TrimSpace(e.logs.String()); output != "" {
		return fmt.Errorf("llama.cpp startup: %w; server output: %s", err, output)
	}
	return fmt.Errorf("llama.cpp startup: %w", err)
}

type startupLog struct {
	mu     sync.Mutex
	tail   []byte
	writer io.Writer
}

func (l *startupLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tail = append(l.tail, p...)
	if len(l.tail) > maxStartupLogBytes {
		l.tail = append([]byte(nil), l.tail[len(l.tail)-maxStartupLogBytes:]...)
	}
	if l.writer != nil {
		// Log output failure must not stop reading a child's output pipe.
		_, _ = l.writer.Write(p)
	}
	return len(p), nil
}

func (l *startupLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.ToValidUTF8(string(l.tail), "")
}
