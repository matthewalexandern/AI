package config

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

type Config struct {
	LlamaPath     string `json:"llama_path"`
	ModelPath     string `json:"model_path"`
	Backend       string `json:"backend"`
	ContextSize   int    `json:"context_size"`
	GpuLayers     int    `json:"gpu_layers"`
	Threads       int    `json:"threads"`
	Port          int    `json:"port"`
	SystemPrompt  string `json:"system_prompt"`
	CognitionMode string `json:"cognition_mode"`
}

func Default() Config {
	threads := runtime.NumCPU()
	if threads > 8 {
		threads = 8
	}
	return Config{Backend: "cpu", ContextSize: 8192, Threads: threads, CognitionMode: "adaptive", SystemPrompt: "You are Mini Fabrics, a local assistant. Give useful, accurate answers. State uncertainty and distinguish evidence from assumptions."}
}

func DefaultHome() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "mini-fabrics"), nil
}

func Load(home string) (Config, error) {
	c := Default()
	f, err := os.Open(filepath.Join(home, "config.json"))
	if err != nil {
		return c, fmt.Errorf("read config: %w; use the installer or 'fabrics init'", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return c, fmt.Errorf("read config: %w", err)
	}
	if len(data) > 1<<20 {
		return c, errors.New("config exceeds 1 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("decode config: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return c, errors.New("config must contain one JSON object")
	}
	c.normalize()
	return c, c.Validate()
}

func (c *Config) normalize() {
	if c.CognitionMode == "" {
		c.CognitionMode = "adaptive"
	}
}

func (c Config) Validate() error {
	// Empty is the adaptive default for callers constructed before this field
	// existed. Persistence always records the explicit normalized mode.
	switch c.CognitionMode {
	case "", "adaptive", "fast", "balanced", "deep":
	default:
		return fmt.Errorf("unsupported cognition_mode %q; use adaptive, fast, balanced, or deep", c.CognitionMode)
	}
	switch c.Backend {
	case "cpu", "cuda", "metal", "vulkan":
	default:
		return fmt.Errorf("unsupported backend %q", c.Backend)
	}
	if c.Backend == "metal" && runtime.GOOS != "darwin" {
		return errors.New("Metal requires macOS")
	}
	if c.ContextSize < 4096 || c.ContextSize > 131072 {
		return errors.New("context_size must be between 4096 and 131072")
	}
	if c.Threads < 1 || c.Threads > 1024 {
		return errors.New("threads must be between 1 and 1024")
	}
	if c.Port < 0 || c.Port > 65535 {
		return errors.New("port must be between 0 and 65535")
	}
	if c.GpuLayers < -1 || c.GpuLayers > 1000 {
		return errors.New("gpu_layers must be between -1 and 1000")
	}
	if c.Backend == "cpu" && c.GpuLayers != 0 {
		return errors.New("CPU backend requires gpu_layers 0")
	}
	if len(c.SystemPrompt) > 16384 {
		return errors.New("system_prompt exceeds 16 KiB")
	}
	info, err := os.Stat(c.LlamaPath)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("llama_path must name an existing executable file: %s", c.LlamaPath)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0 {
		return errors.New("llama_path is not executable")
	}
	return CheckModel(c.ModelPath)
}

func CheckModel(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat GGUF model: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("model must be a regular GGUF file")
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open GGUF model: %w", err)
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("model must be a regular GGUF file")
	}
	// Both supported GGUF versions include two uint64 counts after magic and
	// version. A magic-only prefix is a truncated download, not a valid header.
	var header [24]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return fmt.Errorf("read GGUF header: %w", err)
	}
	version := binary.LittleEndian.Uint32(header[4:])
	if !bytes.Equal(header[:4], []byte("GGUF")) || (version != 2 && version != 3) {
		return errors.New("model is not a supported GGUF v2/v3 file")
	}
	return nil
}

func Save(home string, c Config, force bool) error {
	c.normalize()
	if err := c.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		return err
	}
	dest := filepath.Join(home, "config.json")
	if _, err := os.Lstat(dest); err == nil && !force {
		return errors.New("config already exists; use --force to replace it")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(home, ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if !force {
		return os.Link(f.Name(), dest)
	}
	return os.Rename(f.Name(), dest)
}
