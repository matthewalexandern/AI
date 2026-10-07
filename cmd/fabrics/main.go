package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/matthewalexandern/AI/internal/cognition"
	"github.com/matthewalexandern/AI/internal/config"
	"github.com/matthewalexandern/AI/internal/llama"
	"github.com/matthewalexandern/AI/internal/memory"
)

var version = "0.1.0"

const usage = `Mini Fabrics — a local Go-owned cognitive runtime

Usage: fabrics [--home DIRECTORY] COMMAND [OPTIONS]

  init       Configure an existing llama-server and local GGUF model
  doctor     Check configuration, installed files, and SQLite FTS5
  ask        Run one cognitive turn (--session default, --json)
  chat       Interactive conversation (--session default)
  serve      Run the loopback API (--listen 127.0.0.1:8080)
  remember   Store a memory without starting inference
  recall     Search SQLite FTS5 memories (--limit 6)
  episodes   Read persisted turns and assessments (--session default)
  memory     Inspect, forget, export, backup, or restore local memories
  version    Print runtime version

ask/chat/serve start and stop their own llama.cpp child process.
Memory is shared locally across sessions; conversation history is session-specific.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), terminationSignals()...)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "fabrics:", err)
		os.Exit(1)
	}
}

func flags(name string, out io.Writer) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(out)
	return f
}

func run(ctx context.Context, args []string, in io.Reader, out, errs io.Writer) error {
	defaultHome, err := config.DefaultHome()
	if err != nil {
		return err
	}
	f := flags("fabrics", errs)
	home := f.String("home", defaultHome, "directory for config, memory, model and binaries")
	f.Usage = func() { fmt.Fprint(out, usage) }
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	args = f.Args()
	if len(args) == 0 {
		fmt.Fprint(out, usage)
		return nil
	}
	abs, err := filepath.Abs(*home)
	if err != nil {
		return err
	}
	*home = abs
	command, args := args[0], args[1:]
	switch command {
	case "version":
		if len(args) != 0 {
			return errors.New("version takes no arguments")
		}
		fmt.Fprintln(out, "Mini Fabrics", version)
		return nil
	case "init":
		return initialize(*home, args, errs, out)
	case "doctor":
		f = flags("doctor", errs)
		if err := f.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		if len(f.Args()) != 0 {
			return errors.New("doctor takes no positional arguments")
		}
		return doctor(*home, out)
	case "remember", "recall", "episodes":
		return memoryCommand(ctx, *home, command, args, out, errs)
	case "memory":
		return memoryLifecycleCommand(ctx, *home, args, out, errs)
	case "ask", "chat", "serve":
	default:
		return fmt.Errorf("unknown command %q; use --help", command)
	}
	f = flags(command, errs)
	session := f.String("session", "default", "conversation session identifier")
	asJSON := f.Bool("json", false, "print complete cognitive result as JSON")
	listen := f.String("listen", "127.0.0.1:8080", "loopback API address")
	mode := f.String("mode", "", "adaptive, fast, balanced, or deep (default: configured mode)")
	startupTimeout := f.Duration("startup-timeout", 2*time.Minute, "maximum model loading time (10s..15m)")
	turnTimeout := f.Duration("turn-timeout", 5*time.Minute, "maximum cognitive turn duration (10s..60m)")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if command == "ask" && len(f.Args()) == 0 {
		return errors.New("ask requires a prompt")
	}
	if command != "ask" && len(f.Args()) > 0 {
		return errors.New("unexpected positional arguments")
	}
	if !validSession(*session) {
		return errors.New("session must contain 1 to 128 letters, digits, or ._-:")
	}
	if command == "ask" && strings.TrimSpace(strings.Join(f.Args(), " ")) == "" {
		return errors.New("ask requires a nonempty prompt")
	}
	if command == "serve" {
		if err := validateListen(*listen); err != nil {
			return err
		}
	}
	if *mode != "" && !validCognitionMode(*mode) {
		return errors.New("mode must be adaptive, fast, balanced, or deep")
	}
	if *startupTimeout < 10*time.Second || *startupTimeout > 15*time.Minute {
		return errors.New("startup-timeout must be between 10s and 15m")
	}
	if *turnTimeout < 10*time.Second || *turnTimeout > time.Hour {
		return errors.New("turn-timeout must be between 10s and 60m")
	}
	ctx = context.WithValue(ctx, turnTimeoutKey{}, *turnTimeout)
	cfg, err := config.Load(*home)
	if err != nil {
		return err
	}
	if *mode != "" {
		cfg.CognitionMode = *mode
	}
	store, err := openMemory(*home)
	if err != nil {
		return err
	}
	defer store.Close()
	engine, err := llama.Start(ctx, llama.Options{Executable: cfg.LlamaPath, Model: cfg.ModelPath, Backend: cfg.Backend, ContextSize: cfg.ContextSize, GpuLayers: cfg.GpuLayers, Threads: cfg.Threads, Port: cfg.Port, StartupTimeout: *startupTimeout, Log: errs})
	if err != nil {
		return err
	}
	defer engine.Close()
	budget := cfg.ContextSize - 1536
	maxInput := budget
	if maxInput > 65536 {
		maxInput = 65536
	}
	controller := cognition.New(engine, store, cognition.Config{Mode: cfg.CognitionMode, SystemPrompt: cfg.SystemPrompt, Model: filepath.Base(cfg.ModelPath), ContextBudgetBytes: budget, MaxInputBytes: maxInput})
	switch command {
	case "ask":
		result, err := turn(ctx, controller, *session, strings.Join(f.Args(), " "))
		if err != nil {
			return err
		}
		return printResult(out, result, *asJSON)
	case "chat":
		return chat(ctx, controller, *session, *asJSON, in, out, errs)
	case "serve":
		return serve(ctx, *listen, controller, store, engine, out)
	}
	return nil
}

func initialize(home string, args []string, errs, out io.Writer) error {
	cfg := config.Default()
	f := flags("init", errs)
	llamaPath := f.String("llama", "", "path to llama-server executable")
	modelPath := f.String("model", "", "path to GGUF model")
	f.StringVar(&cfg.Backend, "backend", "cpu", "cpu, cuda, metal, or vulkan")
	f.StringVar(&cfg.CognitionMode, "mode", cfg.CognitionMode, "adaptive, fast, balanced, or deep")
	f.IntVar(&cfg.ContextSize, "context", cfg.ContextSize, "context size")
	f.IntVar(&cfg.Threads, "threads", cfg.Threads, "CPU threads")
	layers := f.Int("gpu-layers", -2, "GPU layers; defaults to 0 for CPU, -1 for GPU")
	force := f.Bool("force", false, "replace existing configuration")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if len(f.Args()) != 0 || *llamaPath == "" || *modelPath == "" {
		return errors.New("init requires --llama PATH and --model PATH")
	}
	var err error
	cfg.LlamaPath, err = filepath.Abs(*llamaPath)
	if err != nil {
		return err
	}
	cfg.ModelPath, err = filepath.Abs(*modelPath)
	if err != nil {
		return err
	}
	cfg.GpuLayers = *layers
	if *layers == -2 {
		if cfg.Backend == "cpu" {
			cfg.GpuLayers = 0
		} else {
			cfg.GpuLayers = -1
		}
	}
	if err := config.Save(home, cfg, *force); err != nil {
		return err
	}
	fmt.Fprintln(out, "Configuration saved to", filepath.Join(home, "config.json"))
	return nil
}

func openMemory(home string) (*memory.Store, error) {
	if err := os.MkdirAll(home, 0700); err != nil {
		return nil, err
	}
	return memory.Open(filepath.Join(home, "memory.db"))
}

func doctor(home string, out io.Writer) error {
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("CREATE VIRTUAL TABLE probe USING fts5(content); INSERT INTO probe VALUES ('memory readiness');"); err != nil {
		return fmt.Errorf("SQLite FTS5: %w", err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM probe WHERE probe MATCH 'readiness'").Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return errors.New("SQLite FTS5 functional check failed")
	}
	fmt.Fprintf(out, "Mini Fabrics %s (%s/%s)\nConfiguration: %s\nBackend: %s\nModel: %s\nllama-server: %s\nSQLite FTS5: passed\nRun ask/chat to verify model inference.\n", version, runtime.GOOS, runtime.GOARCH, filepath.Join(home, "config.json"), cfg.Backend, cfg.ModelPath, cfg.LlamaPath)
	return nil
}

func memoryCommand(ctx context.Context, home, command string, args []string, out, errs io.Writer) error {
	f := flags(command, errs)
	limit := f.Int("limit", 6, "maximum results")
	session := f.String("session", "default", "session for episodes")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *limit < 1 || *limit > 50 {
		return errors.New("limit must be between 1 and 50")
	}
	if command != "episodes" && len(f.Args()) == 0 {
		return fmt.Errorf("%s requires text", command)
	}
	if command == "episodes" && len(f.Args()) != 0 {
		return errors.New("episodes takes no positional arguments")
	}
	store, err := openMemory(home)
	if err != nil {
		return err
	}
	defer store.Close()
	text := strings.Join(f.Args(), " ")
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	switch command {
	case "remember":
		id, err := store.Remember(ctx, text, "explicit")
		if err != nil {
			return err
		}
		return enc.Encode(map[string]int64{"id": id})
	case "recall":
		memories, err := store.Search(ctx, text, *limit)
		if err != nil {
			return err
		}
		return enc.Encode(memories)
	case "episodes":
		episodes, err := store.Episodes(ctx, *session, *limit)
		if err != nil {
			return err
		}
		return enc.Encode(episodes)
	}
	return nil
}

type turnTimeoutKey struct{}

func turnDuration(ctx context.Context) time.Duration {
	if duration, ok := ctx.Value(turnTimeoutKey{}).(time.Duration); ok && duration > 0 {
		return duration
	}
	return 5 * time.Minute
}

func turn(ctx context.Context, c *cognition.Controller, session, input string) (cognition.Result, error) {
	timed, cancel := context.WithTimeout(ctx, turnDuration(ctx))
	defer cancel()
	return c.Run(timed, session, input)
}

func printResult(out io.Writer, result cognition.Result, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	if !result.Assessed {
		_, err := fmt.Fprintf(out, "%s\n\nCognition: %s; assessment skipped.\n", result.Answer, result.Decision.Mode)
		return err
	}
	_, err := fmt.Fprintf(out, "%s\n\nAssessment (model-reported): %.0f%% confidence; %s\n", result.Answer, result.Assessment.Confidence*100, result.Assessment.Notes)
	return err
}

func validCognitionMode(mode string) bool {
	switch mode {
	case "adaptive", "fast", "balanced", "deep":
		return true
	default:
		return false
	}
}

func validateListen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("serve requires a loopback IP such as 127.0.0.1 or ::1")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return errors.New("listen port must be a number between 0 and 65535")
	}
	return nil
}

func serve(ctx context.Context, addr string, c *cognition.Controller, store *memory.Store, engine *llama.Engine, out io.Writer) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: apiHandler(c, store, engine.BaseURL()), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: turnDuration(ctx) + time.Minute, IdleTimeout: 30 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	fmt.Fprintln(out, "Mini Fabrics API listening on", listener.Addr().String())
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			server.Close()
			return err
		}
		return nil
	}
}

func apiHandler(c *cognition.Controller, store *memory.Store, engineURL string) http.Handler {
	mux := http.NewServeMux()
	registerMemoryLifecycleRoutes(mux, store)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, engineURL+"/health", nil)
		client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect denied") }}
		defer client.CloseIdleConnections()
		resp, err := client.Do(req)
		if err != nil {
			jsonReply(w, 503, map[string]string{"status": "inference unavailable"})
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			jsonReply(w, 503, map[string]string{"status": "inference unavailable"})
			return
		}
		var health struct {
			Status string `json:"status"`
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4097))
		if readErr != nil || len(data) > 4096 || json.Unmarshal(data, &health) != nil || health.Status != "ok" {
			jsonReply(w, 503, map[string]string{"status": "inference unavailable"})
			return
		}
		jsonReply(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/chat", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Session string `json:"session"`
			Input   string `json:"input"`
		}
		if err := readJSON(w, r, &body); err != nil {
			jsonReply(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if body.Session == "" {
			body.Session = "default"
		}
		if !validSession(body.Session) {
			jsonReply(w, 400, map[string]string{"error": "session must contain 1 to 128 letters, digits, or ._-:"})
			return
		}
		if strings.TrimSpace(body.Input) == "" || len(body.Input) > 64<<10 || !utf8.ValidString(body.Input) || strings.ContainsRune(body.Input, 0) {
			jsonReply(w, 400, map[string]string{"error": "input must contain 1 to 65536 bytes"})
			return
		}
		result, err := turn(r.Context(), c, body.Session, body.Input)
		if err != nil {
			jsonReply(w, apiErrorStatus(err, http.StatusBadGateway), map[string]string{"error": err.Error()})
			return
		}
		jsonReply(w, 200, result)
	})
	mux.HandleFunc("POST /v1/memory", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Content string `json:"content"`
		}
		if err := readJSON(w, r, &body); err != nil {
			jsonReply(w, 400, map[string]string{"error": err.Error()})
			return
		}
		id, err := store.Remember(r.Context(), body.Content, "explicit")
		if err != nil {
			jsonReply(w, apiErrorStatus(err, http.StatusInternalServerError), map[string]string{"error": err.Error()})
			return
		}
		jsonReply(w, 201, map[string]int64{"id": id})
	})
	mux.HandleFunc("GET /v1/memory", func(w http.ResponseWriter, r *http.Request) {
		m, err := store.Search(r.Context(), r.URL.Query().Get("q"), 6)
		if err != nil {
			jsonReply(w, apiErrorStatus(err, http.StatusInternalServerError), map[string]string{"error": err.Error()})
			return
		}
		jsonReply(w, 200, m)
	})
	mux.HandleFunc("GET /v1/episodes", func(w http.ResponseWriter, r *http.Request) {
		session := r.URL.Query().Get("session")
		if session == "" {
			session = "default"
		}
		items, err := store.Episodes(r.Context(), session, 10)
		if err != nil {
			jsonReply(w, apiErrorStatus(err, http.StatusInternalServerError), map[string]string{"error": err.Error()})
			return
		}
		jsonReply(w, 200, items)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Reject browser-origin requests: this loopback API is for local clients.
		if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			jsonReply(w, 403, map[string]string{"error": "browser-origin requests are not supported"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func apiErrorStatus(err error, fallback int) int {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	case errors.Is(err, context.Canceled):
		return http.StatusRequestTimeout
	case errors.Is(err, cognition.ErrInvalidRequest), errors.Is(err, cognition.ErrContextBudget), errors.Is(err, memory.ErrInvalidInput):
		return http.StatusBadRequest
	default:
		return fallback
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, dest any) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return errors.New("Content-Type must be application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(dest); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("body must contain one JSON object")
	}
	return nil
}

func validSession(session string) bool {
	if len(session) < 1 || len(session) > 128 {
		return false
	}
	for _, ch := range session {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("._-:", ch)) {
			return false
		}
	}
	return true
}

func jsonReply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
