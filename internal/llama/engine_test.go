package llama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matthewalexandern/AI/internal/memory"
)

// TestMain turns the same test executable into a real child server. This tests
// process ownership on every supported OS without a shell or downloaded model.
func TestMain(m *testing.M) {
	if mode := os.Getenv("MINIFABRICS_TEST_LLAMA_CHILD"); mode != "" {
		helperServer(mode)
		return
	}
	os.Exit(m.Run())
}

func helperServer(mode string) {
	if mode == "check-env" {
		for _, name := range []string{"LLAMA_ARG_TOOLS", "LLAMA_ARG_HOST", "LLAMA_ARG_MODEL"} {
			if _, exists := os.LookupEnv(name); exists {
				fmt.Fprintln(os.Stderr, "inherited launch setting reached child")
				os.Exit(7)
			}
		}
		if os.Getenv("CUDA_VISIBLE_DEVICES") != "fixture-device" {
			fmt.Fprintln(os.Stderr, "driver environment was changed")
			os.Exit(7)
		}
	}
	if mode == "ignore-interrupt" {
		signal.Ignore(os.Interrupt)
	}
	if mode == "exit" {
		fmt.Fprintln(os.Stderr, "model initialization failed: fixture diagnostic")
		os.Exit(3)
	}
	args := make(map[string]string)
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "--jinja" {
			args[os.Args[i]] = "true"
			continue
		}
		if i+1 < len(os.Args) {
			args[os.Args[i]] = os.Args[i+1]
			i++
		}
	}
	if args["--host"] != "127.0.0.1" || args["--alias"] != "local" || args["--parallel"] != "1" || args["--model"] == "" || args["--jinja"] != "true" || args["--reasoning-format"] != "deepseek" || args["--log-verbosity"] != "4" {
		fmt.Fprintln(os.Stderr, "invalid child invocation")
		os.Exit(4)
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(args["--host"], args["--port"]))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(5)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if mode == "unhealthy" {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"status":"loading model"}`)
			return
		}
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model           string                           `json:"model"`
			Messages        []struct{ Role, Content string } `json:"messages"`
			Stream          bool                             `json:"stream"`
			MaxTokens       int                              `json:"max_tokens"`
			Template        map[string]bool                  `json:"chat_template_kwargs"`
			ReasoningEffort string                           `json:"reasoning_effort"`
		}
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&request) != nil || request.Model != "local" || request.Stream || request.MaxTokens != 1024 || len(request.Messages) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if enabled, found := request.Template["enable_thinking"]; !found || enabled || request.ReasoningEffort != "low" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		answer, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": "answer: " + request.Messages[len(request.Messages)-1].Content}}}})
		_, _ = w.Write(answer)
	})
	if err := http.Serve(listener, mux); err != nil {
		os.Exit(6)
	}
}

func TestChildEnvironmentOwnsLaunchSettingsAndScopesNativeLibraryPath(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "native backend")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(directory, "llama-server")
	if err := os.WriteFile(executable, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	inherited := []string{
		"PATH=old-binaries", "LD_LIBRARY_PATH=old-libraries", "DYLD_LIBRARY_PATH=user-dyld-choice",
		"LLAMA_ARG_TOOLS=all", "LLAMA_ARG_HOST=0.0.0.0", "LLAMA_ARG_MODEL=unselected.gguf",
		"CUDA_VISIBLE_DEVICES=1", "VK_ICD_FILENAMES=driver.json", "KEEP=value",
	}
	original := append([]string(nil), inherited...)
	for _, platform := range []string{"linux", "windows", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			if runtime.GOOS == "windows" && platform == "linux" {
				t.Skip("Windows drive paths contain the POSIX library-path separator")
			}
			environment, err := childEnvironment(executable, inherited, platform)
			if err != nil {
				t.Fatal(err)
			}
			values := map[string]string{}
			for _, entry := range environment {
				name, value, _ := strings.Cut(entry, "=")
				if strings.HasPrefix(name, "LLAMA_ARG_") {
					t.Fatalf("inherited launch option reached child: %s", name)
				}
				values[name] = value
			}
			wantPath, wantLibraries := "old-binaries", "old-libraries"
			if platform == "windows" {
				wantPath = directory + ";old-binaries"
			}
			if platform == "linux" {
				wantLibraries = directory + ":old-libraries"
			}
			if values["PATH"] != wantPath || values["LD_LIBRARY_PATH"] != wantLibraries || values["DYLD_LIBRARY_PATH"] != "user-dyld-choice" || values["CUDA_VISIBLE_DEVICES"] != "1" || values["VK_ICD_FILENAMES"] != "driver.json" || values["KEEP"] != "value" {
				t.Fatalf("incorrect child environment: %v", values)
			}
			if !reflect.DeepEqual(inherited, original) {
				t.Fatal("child environment mutated inherited environment")
			}
		})
	}
	for _, invalid := range []string{"relative-server", directory, filepath.Join(directory, "missing")} {
		if _, err := childEnvironment(invalid, inherited, "linux"); err == nil {
			t.Fatalf("accepted unvalidated executable %q", invalid)
		}
	}
}

func TestStartChangesOnlyChildEnvironment(t *testing.T) {
	opts := helperOptions(t, "check-env")
	t.Setenv("LLAMA_ARG_TOOLS", "all")
	t.Setenv("LLAMA_ARG_HOST", "0.0.0.0")
	t.Setenv("LLAMA_ARG_MODEL", "unselected.gguf")
	t.Setenv("CUDA_VISIBLE_DEVICES", "fixture-device")
	parent := os.Environ()
	engine, err := Start(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if !reflect.DeepEqual(parent, os.Environ()) {
		t.Fatal("starting llama.cpp changed the parent environment")
	}
	want, err := childEnvironment(opts.Executable, parent, runtime.GOOS)
	if err != nil || !reflect.DeepEqual(engine.cmd.Env, want) {
		t.Fatalf("child did not receive the scoped environment: %v", err)
	}
}

func helperOptions(t *testing.T, mode string) Options {
	t.Helper()
	t.Setenv("MINIFABRICS_TEST_LLAMA_CHILD", mode)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(t.TempDir(), "model with spaces.gguf")
	if err := os.WriteFile(model, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	return Options{Executable: executable, Model: model, Backend: "cpu", ContextSize: 1024, Threads: 1, GpuLayers: 30, StartupTimeout: 5 * time.Second}
}

func TestStartCompleteCloseOwnsChild(t *testing.T) {
	opts := helperOptions(t, "serve")
	engine, err := Start(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if !strings.HasPrefix(engine.BaseURL(), "http://127.0.0.1:") {
		t.Fatalf("non-loopback URL: %s", engine.BaseURL())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	answer, err := engine.Complete(ctx, []memory.Message{{Role: "user", Content: "a real process"}}, .2)
	if err != nil || answer != "answer: a real process" {
		t.Fatalf("Complete = %q, %v", answer, err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	select {
	case <-engine.processDone:
	default:
		t.Fatal("Close returned before reaping child")
	}
	if engine.cmd.ProcessState == nil {
		t.Fatal("child has no completed Wait state")
	}
	if _, err := engine.Complete(ctx, []memory.Message{{Role: "user", Content: "closed"}}, 0); err == nil {
		t.Fatal("inference succeeded after Close")
	}
}

func TestParentCancellationReapsChild(t *testing.T) {
	opts := helperOptions(t, "serve")
	ctx, cancel := context.WithCancel(context.Background())
	engine, err := Start(ctx, opts)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	cancel()
	select {
	case <-engine.processDone:
	case <-time.After(5 * time.Second):
		t.Fatal("parent cancellation did not reap child")
	}
}

func TestCloseKillsUncooperativeChild(t *testing.T) {
	opts := helperOptions(t, "ignore-interrupt")
	engine, err := Start(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-engine.processDone:
	default:
		t.Fatal("uncooperative child was not reaped")
	}
}

func TestStartupFailureIncludesBoundedDiagnostics(t *testing.T) {
	opts := helperOptions(t, "exit")
	engine, err := Start(context.Background(), opts)
	if engine != nil || err == nil || !strings.Contains(err.Error(), "fixture diagnostic") {
		t.Fatalf("Start = %v, %v", engine, err)
	}
	log := &startupLog{}
	_, _ = log.Write([]byte(strings.Repeat("a", 3*maxStartupLogBytes)))
	if len(log.String()) != maxStartupLogBytes {
		t.Fatalf("unbounded startup log: %d bytes", len(log.String()))
	}
}

func TestStartupTimeoutAndCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelled), func(t *testing.T) {
			opts := helperOptions(t, "unhealthy")
			opts.StartupTimeout = 300 * time.Millisecond
			var err error
			opts.Port, err = availablePort(0)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cancelled {
				time.AfterFunc(150*time.Millisecond, cancel)
			}
			engine, err := Start(ctx, opts)
			if engine != nil || err == nil {
				t.Fatalf("Start = %v, %v", engine, err)
			}
			want := context.DeadlineExceeded
			if cancelled {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("startup error = %v, want %v", err, want)
			}
			listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.Port)))
			if err != nil {
				t.Fatalf("startup failure left a listening child: %v", err)
			}
			_ = listener.Close()
		})
	}
}

func TestOptionsAndLoopbackPortValidation(t *testing.T) {
	opts := helperOptions(t, "serve")
	normalized, err := validateOptions(opts)
	if err != nil || normalized.GpuLayers != 0 {
		t.Fatalf("CPU normalization = %+v, %v", normalized, err)
	}
	for _, change := range []func(*Options){
		func(o *Options) { o.Model = "" },
		func(o *Options) { o.Executable = t.TempDir() },
		func(o *Options) { o.Model = filepath.Join(t.TempDir(), "missing") },
		func(o *Options) { o.Backend = "unknown" },
		func(o *Options) { o.Port = 65536 },
		func(o *Options) { o.GpuLayers = -2 },
		func(o *Options) { o.StartupTimeout = -1 },
	} {
		invalid := opts
		change(&invalid)
		if _, err := validateOptions(invalid); err == nil {
			t.Fatalf("accepted invalid options: %+v", invalid)
		}
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if _, err := availablePort(listener.Addr().(*net.TCPAddr).Port); err == nil {
		t.Fatal("accepted a busy loopback port")
	}
}

func fixtureEngine(t *testing.T, handler http.HandlerFunc) *Engine {
	t.Helper()
	server := httptest.NewServer(handler)
	ctx, cancel := context.WithCancel(context.Background())
	engine := &Engine{baseURL: server.URL, client: server.Client(), ctx: ctx, cancel: cancel, processDone: make(chan struct{}), gate: make(chan struct{}, 1)}
	t.Cleanup(func() { _ = engine.Close(); server.Close() })
	return engine
}

func TestCompleteJSONConstrainsOnlyExplicitStructuredRequests(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"confidence":{"type":"number"}},"required":["confidence"],"additionalProperties":false}`)
	requests := make(chan map[string]json.RawMessage, 2)
	engine := fixtureEngine(t, func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- payload
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"confidence\":0.6}"},"finish_reason":"stop"}]}`)
	})
	messages := []memory.Message{{Role: "system", Content: "PHASE: assessment is ordinary text, never an implicit format switch."}, {Role: "user", Content: "Assess this answer."}}
	if _, err := engine.Complete(context.Background(), messages, 0); err != nil {
		t.Fatal(err)
	}
	if _, exists := (<-requests)["response_format"]; exists {
		t.Fatal("ordinary Complete added JSON constraints based on message content")
	}
	answer, err := engine.CompleteJSON(context.Background(), messages, .2, schema)
	if err != nil || answer != `{"confidence":0.6}` {
		t.Fatalf("CompleteJSON = %q, %v", answer, err)
	}
	request := <-requests
	var format responseFormat
	if err := json.Unmarshal(request["response_format"], &format); err != nil {
		t.Fatal(err)
	}
	if format.Type != "json_schema" || format.JSONSchema.Name != "cognitive_assessment" || !format.JSONSchema.Strict || string(format.JSONSchema.Schema) != string(schema) {
		t.Fatalf("structured response format = %s", request["response_format"])
	}
	if string(request["temperature"]) != "0.2" || string(request["max_tokens"]) != "1024" || string(request["stream"]) != "false" || string(request["reasoning_effort"]) != `"low"` {
		t.Fatalf("structured requests changed standard inference options: %v", request)
	}
}

func TestCompleteReturnsOnlyVisibleContent(t *testing.T) {
	for _, content := range []string{"Visible final answer.", "An analysis of the data follows.", "The word chain and the literal <think> tag are ordinary quoted text."} {
		engine := fixtureEngine(t, func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content, "reasoning_content": "PRIVATE_REASONING_FIXTURE_MUST_NOT_ESCAPE"}, "finish_reason": "stop"}}})
		})
		answer, err := engine.Complete(context.Background(), []memory.Message{{Role: "user", Content: "hello"}}, 0)
		if err != nil || answer != content || strings.Contains(answer, "PRIVATE_REASONING_FIXTURE") {
			t.Fatalf("content-only response = %q, %v", answer, err)
		}
	}
}

func TestCompleteJSONRejectsInvalidSchemasBeforeRequest(t *testing.T) {
	var calls atomic.Int32
	engine := fixtureEngine(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{}"}}]}`)
	})
	for _, schema := range []string{"", "null", "[]", `"object"`, "true", "{", "{} {}", "{}" + strings.Repeat(" ", 64<<10)} {
		_, err := engine.CompleteJSON(context.Background(), []memory.Message{{Role: "user", Content: "hello"}}, 0, json.RawMessage(schema))
		if err == nil || !strings.Contains(err.Error(), "schema must be") {
			t.Fatalf("invalid schema accepted (%d bytes): %v", len(schema), err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid schema produced %d inference requests", calls.Load())
	}
}

func TestCompleteRejectsInvalidResponses(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"status", http.StatusServiceUnavailable, `{"error":"busy"}`, "HTTP 503"},
		{"json", http.StatusOK, `{"choices":`, "decode"},
		{"empty", http.StatusOK, `{"choices":[]}`, "empty answer"},
		{"blank", http.StatusOK, `{"choices":[{"message":{"content":"   "}}]}`, "empty answer"},
		{"reasoning only", http.StatusOK, `{"choices":[{"message":{"content":null,"reasoning_content":"PRIVATE_REASONING_FIXTURE_MUST_NOT_ESCAPE"},"finish_reason":"stop"}]}`, "empty answer"},
		{"unparsed analysis", http.StatusOK, `{"choices":[{"message":{"content":"<|channel|>analysis<|message|>PRIVATE_REASONING_FIXTURE_MUST_NOT_ESCAPE<|end|><|channel|>final<|message|>Visible answer"},"finish_reason":"stop"}]}`, "unparsed Harmony analysis"},
		{"truncated", http.StatusOK, `{"choices":[{"message":{"content":"An incomplete answer"},"finish_reason":"length"}]}`, "truncated"},
		{"unexpected termination", http.StatusOK, `{"choices":[{"message":{"content":"An incomplete tool request"},"finish_reason":"tool_calls"}]}`, "did not complete a text answer"},
		{"large", http.StatusOK, strings.Repeat("a", maxResponseBytes+1), "exceeds"},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := fixtureEngine(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			})
			_, err := engine.Complete(context.Background(), []memory.Message{{Role: "user", Content: "hello"}}, 0)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Complete error = %v, want %q", err, test.want)
			}
			if strings.Contains(err.Error(), "PRIVATE_REASONING_FIXTURE") {
				t.Fatal("response error exposed hidden reasoning")
			}
		})
	}
}

func TestHealthRejectsTrailingAndOversizedResponses(t *testing.T) {
	for _, body := range []string{
		`{"status":"ok"} {"status":"loading model"}`,
		`{"status":"ok"}` + strings.Repeat(" ", 4096),
	} {
		engine := fixtureEngine(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, body)
		})
		if err := engine.checkHealth(context.Background()); err == nil {
			t.Fatal("accepted malformed/oversized health response")
		}
	}
}

func TestClosedEngineNeverSendsNewRequest(t *testing.T) {
	var calls atomic.Int32
	engine := fixtureEngine(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"answer"},"finish_reason":"stop"}]}`)
	})
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if _, err := engine.Complete(context.Background(), []memory.Message{{Role: "user", Content: "hello"}}, 0); err == nil {
			t.Fatal("closed engine accepted inference")
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("closed engine sent %d requests", calls.Load())
	}
}

func TestCompleteDeadlineAndQueueCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	engine := fixtureEngine(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		started <- struct{}{}
		<-r.Context().Done()
	})
	firstCtx, firstCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer firstCancel()
	result := make(chan error, 1)
	go func() {
		_, err := engine.Complete(firstCtx, []memory.Message{{Role: "user", Content: "first"}}, 0)
		result <- err
	}()
	<-started
	queuedCtx, queuedCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer queuedCancel()
	_, err := engine.Complete(queuedCtx, []memory.Message{{Role: "user", Content: "queued"}}, 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queue cancellation error = %v", err)
	}
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request deadline error = %v", err)
	}
}

func TestCompleteSerializesRequests(t *testing.T) {
	var active, peak atomic.Int32
	engine := fixtureEngine(t, func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		for current > peak.Load() && !peak.CompareAndSwap(peak.Load(), current) {
		}
		time.Sleep(40 * time.Millisecond)
		active.Add(-1)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"answer"}}]}`)
	})
	results := make(chan error, 3)
	for range 3 {
		go func() {
			_, err := engine.Complete(context.Background(), []memory.Message{{Role: "user", Content: "hello"}}, 0)
			results <- err
		}()
	}
	for range 3 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if peak.Load() != 1 {
		t.Fatalf("concurrent inference requests = %d", peak.Load())
	}
}
