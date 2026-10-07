package config

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func validConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	executable := filepath.Join(dir, "llama server")
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	if err := os.WriteFile(executable, []byte("fixture executable"), 0700); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(dir, "model with spaces.gguf")
	writeGGUF(t, model, 3)
	c := Default()
	c.LlamaPath = executable
	c.ModelPath = model
	return c
}

func writeGGUF(t *testing.T, path string, version uint32) {
	t.Helper()
	// GGUF header: magic, version, tensor count, metadata pair count. This
	// fixture exercises configuration validation; inference loads real models.
	header := make([]byte, 24)
	copy(header, "GGUF")
	binary.LittleEndian.PutUint32(header[4:], version)
	if err := os.WriteFile(path, header, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSaveLoadRoundTripAndPrivateFiles(t *testing.T) {
	c := validConfig(t)
	c.Backend = "vulkan"
	c.GpuLayers = -1
	c.ContextSize = 8192
	c.Threads = 3
	c.Port = 19003
	c.SystemPrompt = "Preserve Unicode: café, 日本語, and actual\nnewlines."
	home := filepath.Join(t.TempDir(), "nested", "mini fabrics")
	if err := Save(home, c, false); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, c) {
		t.Fatalf("round trip changed configuration:\n got %+v\nwant %+v", loaded, c)
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Fatalf("unexpected persistence artifacts: %v, %v", entries, err)
	}
	if runtime.GOOS != "windows" {
		for path, mode := range map[string]os.FileMode{home: 0700, filepath.Join(home, "config.json"): 0600} {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != mode {
				t.Fatalf("permissions for %s: %v, %v; want %o", path, info, err, mode)
			}
		}
	}
}

func TestCognitionModesRoundTrip(t *testing.T) {
	for _, mode := range []string{"adaptive", "fast", "balanced", "deep"} {
		t.Run(mode, func(t *testing.T) {
			c := validConfig(t)
			c.CognitionMode = mode
			home := t.TempDir()
			if err := Save(home, c, false); err != nil {
				t.Fatal(err)
			}
			loaded, err := Load(home)
			if err != nil || !reflect.DeepEqual(loaded, c) {
				t.Fatalf("cognition mode round trip changed configuration: %+v, %v", loaded, err)
			}
		})
	}
}

func TestEmptyCognitionModeUsesAdaptive(t *testing.T) {
	c := validConfig(t)
	c.CognitionMode = ""
	if err := c.Validate(); err != nil {
		t.Fatalf("legacy programmatic configuration rejected: %v", err)
	}
	home := t.TempDir()
	path := filepath.Join(home, "config.json")
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(home)
	if err != nil || loaded.CognitionMode != "adaptive" {
		t.Fatalf("empty persisted mode = %q, %v; want adaptive", loaded.CognitionMode, err)
	}
	if err := Save(home, c, true); err != nil {
		t.Fatal(err)
	}
	encoded, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := json.Unmarshal(encoded, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.CognitionMode != "adaptive" {
		t.Fatalf("Save recorded cognition_mode %q; want explicit adaptive", saved.CognitionMode)
	}
	if c.CognitionMode != "" {
		t.Fatalf("Save changed the caller's configuration: %q", c.CognitionMode)
	}
}

func TestInvalidCognitionModeRejectedBeforePersistence(t *testing.T) {
	for _, mode := range []string{"auto", "FAST", " balanced ", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			c := validConfig(t)
			c.CognitionMode = mode
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "cognition_mode") {
				t.Fatalf("Validate accepted invalid cognition mode %q: %v", mode, err)
			}
			home := filepath.Join(t.TempDir(), "new-config")
			if err := Save(home, c, false); err == nil {
				t.Fatal("Save accepted invalid cognition mode")
			}
			if _, err := os.Stat(home); !os.IsNotExist(err) {
				t.Fatalf("invalid configuration created home directory: %v", err)
			}
		})
	}
}

func TestSaveProtectsExistingConfigAndForceReplaces(t *testing.T) {
	home := t.TempDir()
	c := validConfig(t)
	if err := Save(home, c, false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := c
	changed.SystemPrompt = "A replacement prompt."
	if err := Save(home, changed, false); err == nil {
		t.Fatal("Save replaced an existing config without force")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatalf("failed Save changed existing bytes: %v", err)
	}
	invalid := changed
	invalid.ModelPath = filepath.Join(home, "missing.gguf")
	if err := Save(home, invalid, true); err == nil {
		t.Fatal("Save accepted an invalid replacement model")
	}
	after, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatalf("invalid forced Save changed existing bytes: %v", err)
	}
	if err := Save(home, changed, true); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(home)
	if err != nil || !reflect.DeepEqual(loaded, changed) {
		t.Fatalf("forced replacement = %+v, %v", loaded, err)
	}
}

func TestSaveProtectsDanglingConfigSymlink(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.json")
	target := filepath.Join(home, "not-yet-created.json")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if err := Save(home, validConfig(t), false); err == nil {
		t.Fatal("Save replaced an existing config symlink without force")
	}
	actual, err := os.Readlink(path)
	if err != nil || actual != target {
		t.Fatalf("existing symlink was changed: target %q, %v", actual, err)
	}
}

func TestConcurrentInitializersPublishExactlyOneConfig(t *testing.T) {
	const count = 16
	home := t.TempDir()
	base := validConfig(t)
	type result struct {
		config Config
		err    error
	}
	results := make(chan result, count)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(count)
	for i := range count {
		c := base
		c.SystemPrompt = strings.Repeat("x", i+1)
		go func() {
			ready.Done()
			<-start
			results <- result{config: c, err: Save(home, c, false)}
		}()
	}
	ready.Wait()
	close(start)
	var winner Config
	successes := 0
	for range count {
		result := <-results
		if result.err == nil {
			successes++
			winner = result.config
		}
	}
	if successes != 1 {
		t.Fatalf("create-only initialization published %d configurations; want exactly one", successes)
	}
	loaded, err := Load(home)
	if err != nil || !reflect.DeepEqual(loaded, winner) {
		t.Fatalf("published config does not match sole successful initializer: %+v, %v", loaded, err)
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Fatalf("concurrent initialization left temporary files: %v, %v", entries, err)
	}
}

func TestLoadRejectsInvalidLaunchConfiguration(t *testing.T) {
	base := validConfig(t)
	for _, test := range []struct {
		name   string
		change func(*Config)
	}{
		{"cognition mode", func(c *Config) { c.CognitionMode = "auto-typo" }},
		{"backend", func(c *Config) { c.Backend = "rocm-typo" }},
		{"small context", func(c *Config) { c.ContextSize = 2047 }},
		{"large context", func(c *Config) { c.ContextSize = 131073 }},
		{"zero threads", func(c *Config) { c.Threads = 0 }},
		{"large threads", func(c *Config) { c.Threads = 1025 }},
		{"negative port", func(c *Config) { c.Port = -1 }},
		{"large port", func(c *Config) { c.Port = 65536 }},
		{"negative layers", func(c *Config) { c.GpuLayers = -2; c.Backend = "cuda" }},
		{"large layers", func(c *Config) { c.GpuLayers = 1001; c.Backend = "cuda" }},
		{"CPU offload mismatch", func(c *Config) { c.GpuLayers = 20 }},
		{"long system prompt", func(c *Config) { c.SystemPrompt = strings.Repeat("a", 16385) }},
		{"missing executable", func(c *Config) { c.LlamaPath = filepath.Join(t.TempDir(), "missing-server") }},
		{"executable directory", func(c *Config) { c.LlamaPath = t.TempDir() }},
		{"missing model", func(c *Config) { c.ModelPath = filepath.Join(t.TempDir(), "missing.gguf") }},
		{"model directory", func(c *Config) { c.ModelPath = t.TempDir() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := base
			test.change(&c)
			encoded, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, "config.json"), encoded, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(home); err == nil {
				t.Fatal("Load accepted a configuration that cannot safely launch")
			}
		})
	}
}

func TestExecutablePermissionsAndMetalCompatibility(t *testing.T) {
	c := validConfig(t)
	if runtime.GOOS != "windows" {
		if err := os.Chmod(c.LlamaPath, 0600); err != nil {
			t.Fatal(err)
		}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "not executable") {
			t.Fatalf("non-executable file accepted: %v", err)
		}
		if err := os.Chmod(c.LlamaPath, 0700); err != nil {
			t.Fatal(err)
		}
	}
	c.Backend = "metal"
	c.GpuLayers = -1
	err := c.Validate()
	if runtime.GOOS == "darwin" {
		if err != nil {
			t.Fatalf("Metal rejected on macOS: %v", err)
		}
	} else if err == nil || !strings.Contains(err.Error(), "macOS") {
		t.Fatalf("Metal accepted on %s: %v", runtime.GOOS, err)
	}
}

func TestCheckModelRejectsUnsupportedAndTruncatedFiles(t *testing.T) {
	for _, version := range []uint32{2, 3} {
		path := filepath.Join(t.TempDir(), "supported.gguf")
		writeGGUF(t, path, version)
		if err := CheckModel(path); err != nil {
			t.Fatalf("GGUF v%d rejected: %v", version, err)
		}
	}
	for _, version := range []uint32{0, 1, 4, 0xffffffff} {
		path := filepath.Join(t.TempDir(), "unsupported.gguf")
		writeGGUF(t, path, version)
		if err := CheckModel(path); err == nil {
			t.Fatalf("GGUF v%d accepted", version)
		}
	}
	for _, content := range [][]byte{nil, []byte("GGUF"), []byte("GGML\x03\x00\x00\x00"), []byte("GGUF\x03\x00\x00\x00"), append([]byte("GGUF\x03\x00\x00\x00"), make([]byte, 15)...)} {
		path := filepath.Join(t.TempDir(), "invalid.gguf")
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		if err := CheckModel(path); err == nil {
			t.Fatalf("invalid GGUF accepted: %q", content)
		}
	}
}

func TestLoadRejectsUnknownFieldsTrailingContentAndOversizedFile(t *testing.T) {
	c := validConfig(t)
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		body []byte
	}{
		{"unknown field", append(append([]byte(nil), encoded[:len(encoded)-1]...), []byte(`,"model_paths":"typo"}`)...)},
		{"second object", append(append([]byte(nil), encoded...), []byte(` {"backend":"cuda"}`)...)},
		{"trailing garbage", append(append([]byte(nil), encoded...), []byte(` not-json`)...)},
		{"oversized valid prefix", append(append(append([]byte(nil), encoded...), bytes.Repeat([]byte(" "), 1<<20)...), []byte(` {"backend":"cuda"}`)...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, "config.json"), test.body, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(home); err == nil {
				t.Fatal("Load silently accepted malformed or oversized configuration")
			}
		})
	}
}

func TestLoadRetainsDefaultsForOmittedOptionalFields(t *testing.T) {
	c := validConfig(t)
	encoded, err := json.Marshal(map[string]string{"llama_path": c.LlamaPath, "model_path": c.ModelPath})
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(home)
	if err != nil || !reflect.DeepEqual(loaded, c) {
		t.Fatalf("optional defaults changed: %+v, %v", loaded, err)
	}
	if loaded.CognitionMode != "adaptive" {
		t.Fatalf("legacy configuration defaulted cognition_mode to %q; want adaptive", loaded.CognitionMode)
	}
}
