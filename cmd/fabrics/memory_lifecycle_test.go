package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/matthewalexandern/AI/internal/memory"
)

func TestMemoryLifecycleCLIExportForgetRestoreAndBackup(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	text, err := runCLI(t, home, "remember", "Neptune is my favorite planet")
	if err != nil {
		t.Fatal(err)
	}
	var saved struct{ ID int64 }
	if err := json.Unmarshal([]byte(text), &saved); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(saved.ID, 10)
	exported := filepath.Join(t.TempDir(), "memory.json")
	backup := filepath.Join(t.TempDir(), "memory.sqlite")
	for _, args := range [][]string{{"memory", "inspect", id}, {"memory", "export", exported}, {"memory", "backup", backup}, {"memory", "forget", id}} {
		if _, err := runCLI(t, home, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if result, err := runCLI(t, home, "recall", "Neptune"); err != nil || strings.Contains(result, "favorite") {
		t.Fatalf("forgotten memory remained searchable: %s / %v", result, err)
	}
	if _, err := runCLI(t, home, "memory", "restore", exported); err != nil {
		t.Fatal(err)
	}
	if result, err := runCLI(t, home, "memory", "list"); err != nil || !strings.Contains(result, "Neptune") {
		t.Fatalf("JSON restore failed: %s / %v", result, err)
	}
	restored := filepath.Join(t.TempDir(), "restored-home")
	if _, err := runCLI(t, restored, "memory", "restore", "--replace", backup); err != nil {
		t.Fatal(err)
	}
	if result, err := runCLI(t, restored, "recall", "Neptune"); err != nil || !strings.Contains(result, "Neptune") {
		t.Fatalf("SQLite restore failed: %s / %v", result, err)
	}
	before, err := os.ReadFile(exported)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, home, "memory", "export", exported); err == nil {
		t.Fatal("export overwrote an existing snapshot")
	}
	after, err := os.ReadFile(exported)
	if err != nil || string(after) != string(before) {
		t.Fatal("existing snapshot changed")
	}
}

func TestMemoryLifecycleAPIAndInvalidMode(t *testing.T) {
	home := t.TempDir()
	if _, err := runCLI(t, home, "ask", "--mode", "invalid", "Hello"); err == nil || !strings.Contains(err.Error(), "mode must") {
		t.Fatalf("invalid mode was not rejected before loading inference: %v", err)
	}
	store, err := openMemory(home)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id, err := store.Remember(t.Context(), "favorite planet Neptune", "explicit")
	if err != nil {
		t.Fatal(err)
	}
	handler := apiHandler(nil, store, "")
	path := "/v1/memory/" + strconv.FormatInt(id, 10)
	for _, request := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, path, http.StatusOK},
		{http.MethodGet, "/v1/memories?limit=1", http.StatusOK},
		{http.MethodGet, "/v1/memories?limit=not-a-number", http.StatusBadRequest},
		{http.MethodDelete, path, http.StatusOK},
		{http.MethodGet, path, http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(request.method, request.path, nil))
		if response.Code != request.status {
			t.Fatalf("%s %s: %d %s", request.method, request.path, response.Code, response.Body.String())
		}
	}
	if _, err := store.Inspect(t.Context(), id); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("deleted memory lookup returned %v", err)
	}
}
