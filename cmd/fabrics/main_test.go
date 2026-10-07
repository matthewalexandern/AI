package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matthewalexandern/AI/internal/cognition"
	"github.com/matthewalexandern/AI/internal/config"
	"github.com/matthewalexandern/AI/internal/memory"
)

func runCLI(t *testing.T, home string, args ...string) (string, error) {
	t.Helper()
	var out, errs bytes.Buffer
	args = append([]string{"--home", home}, args...)
	err := run(context.Background(), args, strings.NewReader(""), &out, &errs)
	return out.String(), err
}

func TestMemoryCLIWorksWithoutInferenceConfiguration(t *testing.T) {
	home := filepath.Join(t.TempDir(), "local-runtime")
	text, err := runCLI(t, home, "remember", "Orchid", "prefers", "indirect", "sunlight")
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &saved); err != nil || saved.ID <= 0 {
		t.Fatalf("remember reply = %s, %v", text, err)
	}
	text, err = runCLI(t, home, "recall", "--limit", "1", `"orchid"`)
	if err != nil {
		t.Fatal(err)
	}
	var recalled []memory.Memory
	if err := json.Unmarshal([]byte(text), &recalled); err != nil || len(recalled) != 1 || recalled[0].ID != saved.ID || recalled[0].Kind != "explicit" {
		t.Fatalf("recall reply = %s, %v", text, err)
	}
	store, err := openMemory(home)
	if err != nil {
		t.Fatal(err)
	}
	turn := memory.Turn{Session: "gardening", User: "Watering schedule?", Answer: "Check soil moisture first.", Plan: "Give a bounded recommendation.", Reflection: "Uncertainty remains.", Model: "fixture", Confidence: 0.6, RecallIDs: []int64{saved.ID}}
	if err := store.SaveTurn(context.Background(), turn); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	text, err = runCLI(t, home, "episodes", "--session", "gardening")
	if err != nil {
		t.Fatal(err)
	}
	var episodes []memory.Turn
	if err := json.Unmarshal([]byte(text), &episodes); err != nil || len(episodes) != 1 || episodes[0].Answer != turn.Answer || episodes[0].Reflection != turn.Reflection || !reflect.DeepEqual(episodes[0].RecallIDs, turn.RecallIDs) {
		t.Fatalf("episodes reply = %s, %v", text, err)
	}
	if _, err := os.Stat(filepath.Join(home, "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("memory-only commands unexpectedly required or created inference configuration: %v", err)
	}
}

func TestHelpAndVersionDoNotCreateRuntimeFiles(t *testing.T) {
	for _, args := range [][]string{{}, {"--help"}, {"version"}, {"init", "--help"}, {"doctor", "--help"}, {"ask", "--help"}, {"recall", "--help"}, {"episodes", "--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "absent-runtime")
			var out, errs bytes.Buffer
			err := run(context.Background(), append([]string{"--home", home}, args...), strings.NewReader(""), &out, &errs)
			if err != nil {
				t.Fatal(err)
			}
			if out.Len()+errs.Len() == 0 {
				t.Fatal("help/version produced no output")
			}
			if len(args) == 1 && args[0] == "version" && !strings.Contains(out.String(), "Mini Fabrics "+version) {
				t.Fatalf("version output = %q", out.String())
			}
			if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("help/version created runtime files: %v", err)
			}
		})
	}
}

func TestBadArgumentsFailBeforeInferenceSetup(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"ask", "--session", "invalid session", "hello"}, "session must contain"},
		{[]string{"chat", "--session", ""}, "session must contain"},
		{[]string{"ask", "  "}, "nonempty prompt"},
		{[]string{"doctor", "unexpected"}, "no positional arguments"},
		{[]string{"version", "unexpected"}, "no arguments"},
	} {
		home := filepath.Join(t.TempDir(), "absent-runtime")
		if _, err := runCLI(t, home, test.args...); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%v failed with %v, want %q", test.args, err, test.want)
		}
		if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid arguments created runtime files: %v", err)
		}
	}
}

type watchedReader struct {
	io.ReadCloser
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (r *watchedReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	return r.ReadCloser.Read(p)
}

func (r *watchedReader) Close() error {
	err := r.ReadCloser.Close()
	close(r.closed)
	return err
}

func TestChatCancellationWhileWaitingForTerminalInput(t *testing.T) {
	pipe, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	input := &watchedReader{ReadCloser: pipe, started: make(chan struct{}), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- chat(ctx, nil, "default", false, input, io.Discard, io.Discard) }()
	select {
	case <-input.started:
	case <-time.After(time.Second):
		t.Fatal("chat never started reading input")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("chat cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("chat stayed blocked waiting for input after cancellation")
	}
	select {
	case <-input.closed:
	case <-time.After(time.Second):
		t.Fatal("chat left the input reader blocked after cancellation")
	}
}

func TestChatExitAndEndOfInput(t *testing.T) {
	for _, input := range []string{"", "\n\n", " /exit \n"} {
		if err := chat(context.Background(), nil, "default", false, strings.NewReader(input), io.Discard, io.Discard); err != nil {
			t.Fatalf("chat input %q returned %v", input, err)
		}
	}
}

func TestInitAndDoctorValidateLocalFilesWithoutLaunchingInference(t *testing.T) {
	dir := t.TempDir()
	llamaPath := filepath.Join(dir, "llama-server")
	// This intentionally cannot provide an inference server. Doctor must only
	// inspect configuration/files and perform its real SQLite FTS5 probe.
	if err := os.WriteFile(llamaPath, []byte("not an inference server\n"), 0700); err != nil {
		t.Fatal(err)
	}
	modelPath := filepath.Join(dir, "fixture.gguf")
	header := make([]byte, 24)
	copy(header, "GGUF")
	binary.LittleEndian.PutUint32(header[4:], 3)
	if err := os.WriteFile(modelPath, header, 0600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "runtime")
	if _, err := runCLI(t, home, "init", "--llama", llamaPath, "--model", modelPath, "--threads", "1"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(home)
	if err != nil || cfg.LlamaPath != llamaPath || cfg.ModelPath != modelPath || cfg.Backend != "cpu" || cfg.GpuLayers != 0 {
		t.Fatalf("initialized configuration = %+v, %v", cfg, err)
	}
	text, err := runCLI(t, home, "doctor")
	if err != nil || !strings.Contains(text, "SQLite FTS5: passed") || !strings.Contains(text, "verify model inference") {
		t.Fatalf("doctor reply = %q, %v", text, err)
	}
	if _, err := os.Stat(filepath.Join(home, "memory.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("doctor unexpectedly created the persistent memory store: %v", err)
	}
}

func TestListenValidationHappensBeforeInferenceSetup(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "127.23.4.5:0", "[::1]:65535"} {
		if err := validateListen(addr); err != nil {
			t.Errorf("valid loopback address %q: %v", addr, err)
		}
	}
	for _, addr := range []string{"", "localhost:8080", ":8080", "0.0.0.0:8080", "[::]:8080", "192.168.1.2:8080", "127.0.0.1", "127.0.0.1:", "127.0.0.1:65536", "127.0.0.1:-1", "127.0.0.1:http"} {
		if err := validateListen(addr); err == nil {
			t.Errorf("accepted invalid or nonloopback address %q", addr)
		}
	}
	home := filepath.Join(t.TempDir(), "absent-runtime")
	if _, err := runCLI(t, home, "serve", "--listen", "0.0.0.0:8080"); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("serve validation = %v", err)
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid listen address started runtime setup: %v", err)
	}
}

type apiGenerator struct {
	mu        sync.Mutex
	calls     [][]memory.Message
	failPhase string
}

func (g *apiGenerator) Complete(ctx context.Context, messages []memory.Message, temperature float64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, append([]memory.Message(nil), messages...))
	for _, phase := range []string{"outline", "answer", "assessment"} {
		if !strings.Contains(messages[0].Content, "PHASE: "+phase) {
			continue
		}
		if phase == g.failPhase {
			return "", errors.New("fixture inference unavailable")
		}
		switch phase {
		case "outline":
			return "1. Use the available gardening reference.", nil
		case "answer":
			return "The fern needs indirect sunlight.", nil
		case "assessment":
			return `{"confidence":0.6,"needs_revision":false,"notes":"Model-reported; no external verification.","missing":[]}`, nil
		}
	}
	return "", errors.New("unexpected cognition phase")
}

func (g *apiGenerator) snapshot() [][]memory.Message {
	g.mu.Lock()
	defer g.mu.Unlock()
	result := make([][]memory.Message, len(g.calls))
	for i, messages := range g.calls {
		result[i] = append([]memory.Message(nil), messages...)
	}
	return result
}

func apiFixture(t *testing.T) (*memory.Store, *apiGenerator, http.Handler) {
	t.Helper()
	store, err := memory.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	generator := &apiGenerator{}
	controller := cognition.New(generator, store, cognition.Config{Model: "fixture-model"})
	return store, generator, apiHandler(controller, store, "http://127.0.0.1:1")
}

func apiRequest(handler http.Handler, method, target, body, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	reply := httptest.NewRecorder()
	handler.ServeHTTP(reply, req)
	return reply
}

func decodeAPIReply(t *testing.T, reply *httptest.ResponseRecorder, status int, dest any) {
	t.Helper()
	if reply.Code != status {
		t.Fatalf("HTTP status = %d, want %d; reply=%s", reply.Code, status, reply.Body.String())
	}
	if reply.Header().Get("Content-Type") != "application/json" || reply.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("response headers = %v", reply.Header())
	}
	if dest != nil {
		if err := json.Unmarshal(reply.Body.Bytes(), dest); err != nil {
			t.Fatalf("decode reply %q: %v", reply.Body.String(), err)
		}
	}
}

func TestAPIChatIntegratesRecallCognitionAndSessionHistory(t *testing.T) {
	store, generator, handler := apiFixture(t)
	var saved struct {
		ID int64 `json:"id"`
	}
	decodeAPIReply(t, apiRequest(handler, "POST", "/v1/memory", `{"content":"Fern prefers indirect sunlight."}`, "application/json; charset=utf-8"), 201, &saved)
	if saved.ID <= 0 || len(generator.snapshot()) != 0 {
		t.Fatal("explicit memory unexpectedly invoked inference")
	}
	var memories []memory.Memory
	decodeAPIReply(t, apiRequest(handler, "GET", "/v1/memory?q=fern", "", ""), 200, &memories)
	if len(memories) != 1 || memories[0].ID != saved.ID {
		t.Fatalf("API recall = %+v", memories)
	}
	var first cognition.Result
	decodeAPIReply(t, apiRequest(handler, "POST", "/v1/chat", `{"input":"How do I care for my fern?"}`, "application/json"), 200, &first)
	if first.Answer != "The fern needs indirect sunlight." || first.Assessment.Confidence != 0.6 || first.Revised || len(first.Memories) != 1 || first.Memories[0].ID != saved.ID {
		t.Fatalf("cognitive API response = %+v", first)
	}
	var second cognition.Result
	decodeAPIReply(t, apiRequest(handler, "POST", "/v1/chat", `{"session":"default","input":"What did we discuss about fern?"}`, "application/json"), 200, &second)
	calls := generator.snapshot()
	if len(calls) != 6 {
		t.Fatalf("two turns invoked %d inference phases, want 6", len(calls))
	}
	for i := 3; i < 6; i++ {
		var foundUser, foundAnswer bool
		for _, message := range calls[i] {
			foundUser = foundUser || message.Role == "user" && message.Content == "How do I care for my fern?"
			foundAnswer = foundAnswer || message.Role == "assistant" && message.Content == first.Answer
		}
		if !foundUser || !foundAnswer {
			t.Fatalf("second-turn phase %d omitted session history: %+v", i, calls[i])
		}
	}
	var episodes []memory.Turn
	decodeAPIReply(t, apiRequest(handler, "GET", "/v1/episodes", "", ""), 200, &episodes)
	if len(episodes) != 2 || episodes[0].Session != "default" || episodes[0].Answer != first.Answer || episodes[1].User != "What did we discuss about fern?" {
		t.Fatalf("API episodes = %+v", episodes)
	}
	var other []memory.Turn
	decodeAPIReply(t, apiRequest(handler, "GET", "/v1/episodes?session=other", "", ""), 200, &other)
	if len(other) != 0 {
		t.Fatalf("episodes crossed session boundary: %+v", other)
	}
	messages, err := store.Messages(context.Background(), "default", 10)
	if err != nil || len(messages) != 4 {
		t.Fatalf("persisted API exchange = %+v, %v", messages, err)
	}
}

func TestAPIRejectsInvalidBodiesWithoutInferenceOrWrites(t *testing.T) {
	store, generator, handler := apiFixture(t)
	for _, test := range []struct {
		name, target, body, contentType string
	}{
		{"missing content type", "/v1/chat", `{"input":"hello"}`, ""},
		{"wrong media type", "/v1/chat", `{"input":"hello"}`, "text/plain"},
		{"media type prefix", "/v1/chat", `{"input":"hello"}`, "application/jsonjunk"},
		{"malformed", "/v1/chat", `{"input":`, "application/json"},
		{"unknown field", "/v1/chat", `{"input":"hello","execute":"unexpected"}`, "application/json"},
		{"multiple objects", "/v1/chat", `{"input":"hello"} {"input":"again"}`, "application/json"},
		{"array", "/v1/chat", `[]`, "application/json"},
		{"null", "/v1/chat", `null`, "application/json"},
		{"wrong field type", "/v1/chat", `{"input":42}`, "application/json"},
		{"blank input", "/v1/chat", `{"input":" \n\t"}`, "application/json"},
		{"nul input", "/v1/chat", `{"input":"a\u0000b"}`, "application/json"},
		{"invalid session", "/v1/chat", `{"session":"with space","input":"hello"}`, "application/json"},
		{"long session", "/v1/chat", `{"session":"` + strings.Repeat("a", 129) + `","input":"hello"}`, "application/json"},
		{"oversized input", "/v1/chat", `{"input":"` + strings.Repeat("a", (64<<10)+1) + `"}`, "application/json"},
		{"oversized body", "/v1/chat", `{"input":"` + strings.Repeat("a", (256<<10)+1) + `"}`, "application/json"},
		{"memory unknown field", "/v1/memory", `{"content":"hello","role":"system"}`, "application/json"},
		{"empty memory", "/v1/memory", `{"content":""}`, "application/json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var reply map[string]string
			decodeAPIReply(t, apiRequest(handler, "POST", test.target, test.body, test.contentType), 400, &reply)
			if reply["error"] == "" {
				t.Fatalf("missing rejection explanation: %+v", reply)
			}
		})
	}
	if len(generator.snapshot()) != 0 {
		t.Fatal("invalid request invoked inference")
	}
	messages, err := store.Messages(context.Background(), "default", 10)
	if err != nil || len(messages) != 0 {
		t.Fatalf("invalid request persisted a message: %+v, %v", messages, err)
	}
	memories, err := store.Search(context.Background(), "hello", 10)
	if err != nil || len(memories) != 0 {
		t.Fatalf("invalid request persisted a memory: %+v, %v", memories, err)
	}
}

func TestAPIRejectsBrowserOrigins(t *testing.T) {
	_, generator, handler := apiFixture(t)
	for _, headers := range []map[string]string{{"Origin": "https://example.invalid"}, {"Origin": "null"}, {"Sec-Fetch-Site": "cross-site"}} {
		req := httptest.NewRequest("POST", "/v1/chat", strings.NewReader(`{"input":"hello"}`))
		req.Header.Set("Content-Type", "application/json")
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		reply := httptest.NewRecorder()
		handler.ServeHTTP(reply, req)
		decodeAPIReply(t, reply, 403, nil)
	}
	if len(generator.snapshot()) != 0 {
		t.Fatal("browser-origin request invoked inference")
	}
}

func TestAPIFailedInferenceReturnsNoPartialTurn(t *testing.T) {
	store, generator, handler := apiFixture(t)
	generator.failPhase = "answer"
	decodeAPIReply(t, apiRequest(handler, "POST", "/v1/chat", `{"input":"fern question"}`, "application/json"), 502, nil)
	if len(generator.snapshot()) != 2 {
		t.Fatal("failed inference did not stop at the failing phase")
	}
	turns, err := store.Episodes(context.Background(), "default", 10)
	if err != nil || len(turns) != 0 {
		t.Fatalf("failed inference persisted an assessment: %+v, %v", turns, err)
	}
	messages, err := store.Messages(context.Background(), "default", 10)
	if err != nil || len(messages) != 0 {
		t.Fatalf("failed inference persisted messages: %+v, %v", messages, err)
	}
}

func TestAPIReportsConfiguredInputAndContextLimitsAsClientErrors(t *testing.T) {
	store, generator, _ := apiFixture(t)
	for _, cfg := range []cognition.Config{
		{MaxInputBytes: 8},
		{ContextBudgetBytes: 32},
	} {
		handler := apiHandler(cognition.New(generator, store, cfg), store, "http://127.0.0.1:1")
		reply := apiRequest(handler, "POST", "/v1/chat", `{"input":"Please explain fern care."}`, "application/json")
		decodeAPIReply(t, reply, http.StatusBadRequest, nil)
		if !strings.Contains(reply.Body.String(), "limit") && !strings.Contains(reply.Body.String(), "budget") {
			t.Fatalf("missing limit explanation: %s", reply.Body.String())
		}
	}
	if len(generator.snapshot()) != 0 {
		t.Fatal("over-budget request invoked inference")
	}
}

func TestAPICancellationAndDeadlinesHaveDistinctStatus(t *testing.T) {
	_, generator, handler := apiFixture(t)
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		want := http.StatusRequestTimeout
		if deadline {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			want = http.StatusGatewayTimeout
		} else {
			cancel()
		}
		req := httptest.NewRequest("POST", "/v1/chat", strings.NewReader(`{"input":"hello"}`)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		reply := httptest.NewRecorder()
		handler.ServeHTTP(reply, req)
		cancel()
		decodeAPIReply(t, reply, want, nil)
	}
	if len(generator.snapshot()) != 0 {
		t.Fatal("canceled request invoked inference")
	}
}

func TestAPIMemoryStorageFailuresAreServerErrors(t *testing.T) {
	store, _, handler := apiFixture(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ method, path, body string }{
		{"POST", "/v1/memory", `{"content":"A valid note."}`},
		{"GET", "/v1/memory?q=note", ""},
		{"GET", "/v1/episodes", ""},
	} {
		reply := apiRequest(handler, test.method, test.path, test.body, "application/json")
		decodeAPIReply(t, reply, http.StatusInternalServerError, nil)
	}
}

func TestAPIHealthUsesActualInferenceReadiness(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"ready", 200, `{"status":"ok"}`, 200},
		{"loading status", 503, `{"status":"loading model"}`, 503},
		{"loading JSON", 200, `{"status":"loading model"}`, 503},
		{"malformed JSON", 200, `not JSON`, 503},
		{"trailing JSON", 200, `{"status":"ok"} {}`, 503},
		{"oversized response", 200, `{"status":"ok","padding":"` + strings.Repeat("x", 4096) + `"}`, 503},
		{"redirect", 302, "", 503},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.Path != "/health" {
					t.Errorf("unexpected readiness request: %s %s", r.Method, r.URL.Path)
				}
				if test.status == 302 {
					w.Header().Set("Location", "/redirect-target")
				}
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer fixture.Close()
			handler := apiHandler(nil, nil, fixture.URL)
			var reply map[string]string
			decodeAPIReply(t, apiRequest(handler, "GET", "/health", "", ""), test.want, &reply)
			if calls.Load() != 1 {
				t.Fatalf("readiness request count = %d; redirects must not be followed", calls.Load())
			}
			if (reply["status"] == "ok") != (test.want == 200) {
				t.Fatalf("readiness reply = %+v", reply)
			}
		})
	}
	t.Run("connection failure", func(t *testing.T) {
		fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"status":"ok"}`) }))
		url := fixture.URL
		fixture.Close()
		decodeAPIReply(t, apiRequest(apiHandler(nil, nil, url), "GET", "/health", "", ""), 503, nil)
	})
}
