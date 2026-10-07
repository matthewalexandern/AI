// Mini Fabrics installer. This file uses only the Go standard library and can
// build a native installer for macOS, Linux, or Windows (including PowerShell).
// Release packaging embeds the runtime source through linker variables below.
package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	llamaCommit             = "8345f333951c661d166b00e6f9362e553768f292" // ggml-org/llama.cpp v0.6.0 / b11429
	llamaSHA256             = "84050e783dbd0c4a4678f90f253b0881df04e90a98c20f73ae3709751c4a4850"
	llamaURL                = "https://codeload.github.com/ggml-org/llama.cpp/tar.gz/" + llamaCommit
	llamaBuildProfile       = "static-host-native-v2"
	goVersion               = "1.27.1"
	maxArchiveBytes   int64 = 2 << 30
	maxDownloadBytes  int64 = 64 << 30
)

// Filled by release packaging. A naked `go run installer/install.go` requires
// --source; distributed installers include the exact source used for release.
var runtimeArchiveBase64 string
var runtimeArchiveSHA256 string
var modelCatalogBase64 string
var nativeArchiveBase64 string
var nativeArchiveSHA256 string
var nativeManifestBase64 string

var goArchiveHashes = map[string]string{
	"darwin-amd64":  "8f8f52c6649542cf027bbc9b9c68d1ec042f9f34808a40413f0b8b3f66f3caa4",
	"darwin-arm64":  "ee215d57e0ec269c60cc9ceca68e6bda321ba9ee5afe24f4b0988703c2d87d12",
	"linux-amd64":   "63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445",
	"linux-arm64":   "3450b45a3f9ee8568792736a5c5e70a1f2e9b36c35a8f74958c03e51d7d92bec",
	"windows-amd64": "a3911b5e0e1b1053f25ed0675f4c1c6aad1e2bfcf253df2b9be4caabd2edd95d",
	"windows-arm64": "13b69b87bb0e83f96bc68560a8cace7f0343b1e03469f1110ea18d17e3234069",
}

type model struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size,omitempty"`
	Repository string `json:"repository,omitempty"`
	Filename   string `json:"filename,omitempty"`
	Revision   string `json:"revision,omitempty"`
}

func modelCandidates() []model {
	return []model{
		{Name: "gpt-oss-20b", Repository: "ggml-org/gpt-oss-20b-GGUF", Filename: "gpt-oss-20b-MXFP4.gguf", Size: 12109566624},
		{Name: "gpt-oss-120b", Repository: "ggml-org/gpt-oss-120b-GGUF", Filename: "gpt-oss-120b-MXFP4.gguf", Size: 63387346208},
		{Name: "qwen2.5-0.5b", Repository: "Qwen/Qwen2.5-0.5B-Instruct-GGUF", Filename: "qwen2.5-0.5b-instruct-q4_k_m.gguf", Size: 491400032},
		{Name: "qwen2.5-1.5b", Repository: "Qwen/Qwen2.5-1.5B-Instruct-GGUF", Filename: "qwen2.5-1.5b-instruct-q4_k_m.gguf", Size: 1117320736},
		{Name: "qwen2.5-3b", Repository: "Qwen/Qwen2.5-3B-Instruct-GGUF", Filename: "qwen2.5-3b-instruct-q4_k_m.gguf", Size: 2104932768},
	}
}

func completeCatalog(pinned []model) []model {
	all := modelCandidates()
	for _, m := range pinned {
		replaced := false
		for n := range all {
			if all[n].Name == m.Name {
				all[n] = m
				replaced = true
				break
			}
		}
		if !replaced {
			all = append(all, m)
		}
	}
	return all
}

type options struct {
	prefix, model, modelURL, modelSHA, source, backend string
	nonInteractive, skipModel, listModels, doctor      bool
	buildFromSource, installDeps, allowLowMemory       bool
	jobs                                               int
}

type hardware struct {
	OS, Arch, PackageManager    string
	NVIDIA, CUDA, Vulkan, Metal bool
	CMake, CXX, Go              bool
}

type backendPlan struct{ Name, Reason string }

type installer struct {
	opts       options
	client     *http.Client
	out        io.Writer
	in         io.Reader
	lookup     func(string) (string, error)
	native     *nativeManifest
	nativeRoot string
	capacity   memoryCapacity
	backend    string
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "Installation failed:", err)
		os.Exit(1)
	}
}

func run(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("mini-fabrics-install", flag.ContinueOnError)
	fs.SetOutput(out)
	var opts options
	fs.StringVar(&opts.prefix, "prefix", "", "installation directory (default: user config directory/mini-fabrics)")
	fs.StringVar(&opts.model, "model", "", "catalog model name, auto recommendation, or path to an existing GGUF model")
	fs.StringVar(&opts.modelURL, "model-url", "", "custom HTTPS GGUF download URL (requires --model-sha256)")
	fs.StringVar(&opts.modelSHA, "model-sha256", "", "trusted SHA256 of the model supplied with --model-url")
	fs.StringVar(&opts.source, "source", "", "local Mini Fabrics source checkout; overrides bundled source")
	fs.StringVar(&opts.backend, "backend", "auto", "auto, cpu, cuda, metal, or vulkan")
	fs.BoolVar(&opts.nonInteractive, "non-interactive", false, "never prompt; select --model, --model-url, or --skip-model")
	fs.BoolVar(&opts.skipModel, "skip-model", false, "install binaries without downloading a model; preserves existing model configuration")
	fs.BoolVar(&opts.listModels, "list-models", false, "list bundled, revision-pinned model choices and exit")
	fs.BoolVar(&opts.doctor, "doctor", false, "show detected hardware, required tools, and install plan without changing installed files")
	fs.BoolVar(&opts.buildFromSource, "build-from-source", false, "build runtime and llama.cpp from source instead of using the embedded native release")
	fs.BoolVar(&opts.installDeps, "install-deps", false, "explicitly allow source-build prerequisite installation through the detected package manager")
	fs.BoolVar(&opts.allowLowMemory, "allow-low-memory", false, "allow an explicitly selected model despite a low-memory estimate")
	fs.IntVar(&opts.jobs, "jobs", defaultJobs(), "parallel compiler jobs (1..64; default limited to four)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if opts.jobs < 1 || opts.jobs > 64 {
		return errors.New("--jobs must be between 1 and 64")
	}
	if opts.prefix == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return fmt.Errorf("locating user config directory: %w", err)
		}
		opts.prefix = filepath.Join(configDir, "mini-fabrics")
	}
	var err error
	opts.prefix, err = filepath.Abs(opts.prefix)
	if err != nil {
		return err
	}
	inst := installer{opts: opts, out: out, in: in, lookup: exec.LookPath,
		client: &http.Client{Timeout: 6 * time.Hour, CheckRedirect: secureRedirect}}
	catalog, err := loadCatalog(modelCatalogBase64)
	if err != nil {
		return err
	}
	catalog = completeCatalog(catalog)
	if opts.listModels {
		for _, m := range catalog {
			if m.URL == "" {
				fmt.Fprintf(out, "%s\t%s\tresolved and pinned from official Hugging Face metadata when selected\n", m.Name, m.Repository)
			} else {
				fmt.Fprintf(out, "%s\t%s\tSHA256 %s\n", m.Name, humanBytes(m.Size), m.SHA256)
			}
		}
		if len(catalog) == 0 {
			fmt.Fprintln(out, "No hosted model catalog is embedded in this build.")
		}
		fmt.Fprintln(out, "Select a local GGUF with --model /path/model.gguf, or a trusted HTTPS download with --model-url URL --model-sha256 SHA256.")
		return nil
	}
	hw := detectHardware(inst.lookup)
	if opts.doctor {
		manager := hw.PackageManager
		if manager == "" {
			manager = "none detected"
		}
		fmt.Fprintf(out, "Package manager: %s\n", manager)
	}
	if !opts.buildFromSource && opts.source == "" && nativeManifestBase64 == "" && nativeArchiveBase64 != "" {
		return errors.New("bundled native archive is missing its release manifest")
	}
	if !opts.buildFromSource && opts.source == "" && nativeManifestBase64 != "" {
		inst.native, err = loadNativeManifest(nativeManifestBase64, hw.OS, hw.Arch)
		if err != nil {
			return err
		}
		inst.nativeRoot, err = os.MkdirTemp("", "mini-fabrics-native-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(inst.nativeRoot)
		if err := extractNativePayload(nativeArchiveBase64, nativeArchiveSHA256, inst.nativeRoot, inst.native); err != nil {
			return err
		}
	}
	helper := ""
	if inst.native != nil {
		if _, ok := inst.native.Files["bin/fabrics-system-info"]; ok {
			helper = filepath.Join(inst.nativeRoot, "bin", "fabrics-system-info")
		}
	}
	inst.capacity = detectCapacity(hw, helper)
	if strings.HasPrefix(inst.capacity.Source, "native Swift/") {
		hw.Metal = inst.capacity.Metal
	}
	var plan backendPlan
	if inst.native != nil {
		plan, err = chooseNativeBackend(opts.backend, hw, inst.native)
	} else {
		plan, err = chooseBackend(opts.backend, hw)
	}
	if err != nil {
		return err
	}
	inst.backend = plan.Name
	if inst.native != nil {
		llama := filepath.Join(inst.nativeRoot, filepath.FromSlash(inst.native.BackendPaths[plan.Name]))
		if err := inst.command("", nativeCommandEnv(llama), llama, "--version"); err != nil {
			return fmt.Errorf("bundled backend cannot run on this host: %w", err)
		}
		fabrics := filepath.Join(inst.nativeRoot, "bin", exeName("fabrics"))
		if err := inst.command("", nativeCommandEnv(fabrics), fabrics, "--help"); err != nil {
			return fmt.Errorf("bundled runtime cannot run on this host: %w", err)
		}
	}
	fmt.Fprintf(out, "Mini Fabrics: %s/%s; backend %s (%s)\nInstallation directory: %s\nPinned llama.cpp: v0.6.0 (b11429) / %s\n", hw.OS, hw.Arch, plan.Name, plan.Reason, opts.prefix, llamaCommit)
	reportCapacity(out, inst.capacity, catalog, plan.Name)
	if opts.doctor {
		if inst.native != nil {
			fmt.Fprintf(out, "Native release %s verified: runtime, inference variants, dependencies and licenses bundled. No Go/CMake/C++ installation is required.\n", inst.native.Version)
			fmt.Fprintln(out, "Required network: selected model host and its HTTPS redirects only; a local GGUF or --skip-model needs no downloads.")
		} else {
			fmt.Fprintf(out, "Source-build tools: CMake=%t C++ compiler=%t Go=%t; NVIDIA=%t CUDA compiler=%t Vulkan shader compiler=%t Metal=%t\n", hw.CMake, hw.CXX, hw.Go, hw.NVIDIA, hw.CUDA, hw.Vulkan, hw.Metal)
			fmt.Fprintf(out, "Build parallelism: %d; Go bootstrap version: %s (official SHA256 pinned)\n", opts.jobs, goVersion)
			if !hw.CMake || !hw.CXX {
				fmt.Fprintln(out, toolGuidance(hw.OS, hw.PackageManager))
			}
			if opts.source == "" && runtimeArchiveBase64 == "" {
				fmt.Fprintln(out, "Runtime source: missing; use --source CHECKOUT or a complete native release installer.")
			} else {
				fmt.Fprintln(out, "Runtime source: available")
			}
			fmt.Fprintln(out, "Required source-build network: codeload.github.com; go.dev and dl.google.com for Go bootstrap; proxy.golang.org and sum.golang.org for locked Go dependencies; selected model host and HTTPS redirects.")
			if opts.installDeps {
				fmt.Fprintln(out, "--install-deps requested; doctor reports only and does not run package managers.")
			}
		}
		return nil
	}
	if hw.OS != "linux" && hw.OS != "darwin" && hw.OS != "windows" {
		return fmt.Errorf("unsupported operating system %s", hw.OS)
	}
	selected, local, err := inst.selectModel(catalog)
	if err != nil {
		return err
	}
	if local != "" {
		info, err := os.Stat(local)
		if err != nil {
			return err
		}
		if err := inst.checkModelCapacity(model{Name: filepath.Base(local), Size: info.Size()}); err != nil {
			return err
		}
	}
	if inst.native == nil {
		if opts.source == "" && runtimeArchiveBase64 == "" {
			return errors.New("no native runtime or source is bundled; use --source CHECKOUT or obtain a complete release installer")
		}
		if _, reusable := reusableLlama(opts.prefix, plan.Name); (!hw.CMake || !hw.CXX) && !reusable {
			if opts.installDeps {
				if err := inst.installDependencies(hw); err != nil {
					return err
				}
				hw = detectHardware(inst.lookup)
			}
			if !hw.CMake || !hw.CXX {
				return fmt.Errorf("source mode requires CMake and a C++ compiler. %s Rerun with --install-deps to opt into supported prerequisite installation, or use a complete native release.", toolGuidance(hw.OS, hw.PackageManager))
			}
		}
	}

	return inst.install(plan, selected, local)
}

func defaultJobs() int {
	n := runtime.NumCPU()
	if n > 4 {
		n = 4
	}
	if n < 1 {
		n = 1
	}
	return n
}

func secureRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	if req.URL.Scheme != "https" || req.URL.User != nil {
		return errors.New("download redirected to an insecure URL")
	}
	return nil
}

func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("invalid download URL")
	}
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("download URLs must use HTTPS, contain a hostname, and have no credentials or fragment")
	}
	return nil
}

func validHash(hash string) bool {
	b, e := hex.DecodeString(hash)
	return e == nil && len(b) == sha256.Size
}

func loadCatalog(encoded string) ([]model, error) {
	if encoded == "" {
		return nil, nil
	}
	b, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("invalid bundled model catalog: %w", err)
	}
	var models []model
	if err := json.Unmarshal(b, &models); err != nil {
		return nil, err
	}
	names := make(map[string]bool)
	for _, m := range models {
		if m.Name == "" || strings.ContainsAny(m.Name, `/\:`) || names[m.Name] || !validHash(m.SHA256) || m.Size < 0 {
			return nil, errors.New("invalid bundled model catalog entry")
		}
		if err := validateURL(m.URL); err != nil {
			return nil, err
		}
		u, _ := url.Parse(m.URL)
		if u.Hostname() == "huggingface.co" {
			parts := strings.Split(strings.Trim(u.Path, "/"), "/")
			if len(parts) < 5 || parts[2] != "resolve" || len(parts[3]) != 40 {
				return nil, errors.New("Hugging Face catalog models require an immutable 40-character revision")
			}
			if _, err := hex.DecodeString(parts[3]); err != nil {
				return nil, errors.New("invalid Hugging Face revision")
			}
		}
		names[m.Name] = true
	}
	return models, nil
}

func detectHardware(lookup func(string) (string, error)) hardware {
	h := hardware{OS: runtime.GOOS, Arch: runtime.GOARCH}
	var osRelease []byte
	if h.OS == "linux" {
		osRelease, _ = os.ReadFile("/etc/os-release")
	}
	h.PackageManager = detectPackageManager(h.OS, string(osRelease), lookup)
	has := func(name string) bool { _, e := lookup(name); return e == nil }
	h.CMake = has("cmake")
	h.Go = has("go")
	h.CXX = has("c++") || has("g++") || has("clang++") || has("cl")
	h.NVIDIA = has("nvidia-smi") && commandSucceeds("nvidia-smi", "--query-gpu=name", "--format=csv,noheader")
	h.CUDA = has("nvcc")
	h.Vulkan = has("glslc")
	h.Metal = h.OS == "darwin" && h.Arch == "arm64"
	return h
}

// os-release is data, never a script to source or execute. ID takes precedence
// over ID_LIKE; an incidental foreign manager must not override the distro.
func detectPackageManager(goos, osRelease string, lookup func(string) (string, error)) string {
	var candidates []string
	switch goos {
	case "darwin":
		candidates = []string{"brew"}
	case "windows":
		candidates = []string{"winget", "choco", "scoop"}
	case "linux":
		fields := make(map[string]string)
		for _, line := range strings.Split(osRelease, "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
			key = strings.TrimSpace(key)
			if !ok || (key != "ID" && key != "ID_LIKE") {
				continue
			}
			value = strings.TrimSpace(value)
			if strings.HasPrefix(value, `"`) {
				var err error
				value, err = strconv.Unquote(value)
				if err != nil {
					continue
				}
			} else if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
				value = value[1 : len(value)-1]
			}
			fields[key] = value
		}
		for _, id := range append([]string{fields["ID"]}, strings.Fields(fields["ID_LIKE"])...) {
			switch strings.ToLower(id) {
			case "debian", "ubuntu", "linuxmint", "pop", "neon", "kali", "raspbian":
				candidates = []string{"apt-get", "apt"}
			case "rhel", "fedora", "centos", "rocky", "almalinux", "ol", "scientific":
				candidates = []string{"dnf", "yum", "microdnf"}
			}
			if len(candidates) != 0 {
				break
			}
		}
		if len(candidates) == 0 {
			candidates = []string{"apt-get", "apt", "dnf", "yum", "microdnf"}
		}
	}
	for _, manager := range candidates {
		if _, err := lookup(manager); err == nil {
			return manager
		}
	}
	return ""
}

func commandSucceeds(name string, args ...string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Run() == nil
}

func chooseBackend(request string, h hardware) (backendPlan, error) {
	switch request {
	case "cpu":
		return backendPlan{"cpu", "CPU explicitly selected"}, nil
	case "cuda":
		if !h.NVIDIA || !h.CUDA {
			return backendPlan{}, errors.New("--backend cuda requires a working NVIDIA driver and nvcc CUDA toolkit; install the toolkit or select cpu")
		}
		return backendPlan{"cuda", "NVIDIA driver and CUDA toolkit detected"}, nil
	case "metal":
		if h.OS != "darwin" {
			return backendPlan{}, errors.New("--backend metal is supported only on macOS")
		}
		return backendPlan{"metal", "macOS Metal explicitly selected"}, nil
	case "vulkan":
		if !h.Vulkan {
			return backendPlan{}, errors.New("--backend vulkan requires the Vulkan SDK, glslc, development headers, and a working Vulkan device")
		}
		return backendPlan{"vulkan", "Vulkan explicitly selected; CMake will validate SDK availability"}, nil
	case "auto":
		if h.Metal {
			return backendPlan{"metal", "Apple silicon Metal detected"}, nil
		}
		if h.NVIDIA && h.CUDA {
			return backendPlan{"cuda", "NVIDIA driver and CUDA toolkit detected"}, nil
		}
		if h.NVIDIA {
			return backendPlan{"cpu", "NVIDIA GPU detected but CUDA toolkit is missing; install it and rerun with --backend cuda"}, nil
		}
		// Shader tools alone cannot establish a working Vulkan GPU. Vulkan is
		// available explicitly after the user installs the platform SDK/driver.
		return backendPlan{"cpu", "no fully configured GPU backend detected"}, nil
	default:
		return backendPlan{}, fmt.Errorf("unknown backend %q (choose auto, cpu, cuda, metal, vulkan)", request)
	}
}

func toolGuidance(goos, manager string) string {
	switch goos {
	case "darwin":
		cmake := "Install CMake for macOS from https://cmake.org/download/ and add its bin directory to PATH."
		if manager == "brew" {
			cmake = "Install CMake with: brew install cmake."
		}
		return "Install Apple's compiler with: xcode-select --install. " + cmake + " Metal uses the macOS SDK."
	case "windows":
		cmake := "Install CMake for Windows from https://cmake.org/download/ and select the option to add CMake to PATH."
		switch manager {
		case "winget":
			cmake = "In PowerShell install CMake with: winget install --exact --id Kitware.CMake."
		case "choco":
			cmake = "In an administrator PowerShell install CMake with: choco install cmake. Ensure its bin directory is on PATH."
		case "scoop":
			cmake = "In PowerShell install CMake with: scoop install cmake."
		}
		return cmake + " Install Visual Studio Build Tools from https://visualstudio.microsoft.com/visual-cpp-build-tools/ with the Desktop development with C++ workload, then open Developer PowerShell and rerun this installer."
	case "linux":
		var command string
		switch manager {
		case "apt-get", "apt":
			command = manager + " update && " + manager + " install -y build-essential cmake"
		case "dnf", "yum", "microdnf":
			command = manager + " install -y gcc gcc-c++ cmake make"
		}
		if command != "" {
			return "Run with administrator privileges: " + command + ". CUDA requires the vendor toolkit; Vulkan requires SDK development headers and glslc."
		}
		return "Install CMake, a C++17 compiler, and make using your distribution's supported installation instructions; ensure cmake and c++ are on PATH. CUDA requires the vendor toolkit; Vulkan requires SDK development headers and glslc."
	default:
		return "Install CMake and a C++17 compiler using your platform's supported installation instructions, then add them to PATH."
	}
}

func (i *installer) selectModel(catalog []model) (*model, string, error) {
	o := i.opts
	if o.skipModel {
		if o.model != "" || o.modelURL != "" || o.modelSHA != "" {
			return nil, "", errors.New("--skip-model cannot be combined with a model selection")
		}
		return nil, "", nil
	}
	if o.modelURL != "" {

		if o.model != "" {
			return nil, "", errors.New("choose --model or --model-url, not both")
		}
		if err := validateURL(o.modelURL); err != nil {
			return nil, "", err
		}
		if !validHash(o.modelSHA) {
			return nil, "", errors.New("--model-url requires --model-sha256 with a trusted 64-character SHA256")
		}
		return &model{Name: "custom-" + strings.ToLower(o.modelSHA[:12]), URL: o.modelURL, SHA256: strings.ToLower(o.modelSHA)}, "", nil
	}
	if o.modelSHA != "" {
		return nil, "", errors.New("--model-sha256 requires --model-url")
	}
	if o.model == "auto" {
		selected, reason := recommendModel(catalog, i.capacity, i.backend)
		if selected == nil {
			return nil, "", errors.New("automatic model selection unavailable: " + reason)
		}
		fmt.Fprintf(i.out, "Selected %s: %s.\n", selected.Name, reason)
		return selected, "", nil
	}
	if o.model != "" {
		for _, m := range catalog {
			if o.model == m.Name || (m.Repository != "" && o.model == m.Repository) {
				return &m, "", nil
			}
		}
		p, err := filepath.Abs(o.model)
		if err != nil {
			return nil, "", err
		}
		if err := checkGGUF(p); err != nil {
			return nil, "", fmt.Errorf("unknown catalog model or invalid local GGUF %q: %w", o.model, err)
		}
		return nil, p, nil
	}
	if o.nonInteractive {
		return nil, "", errors.New("non-interactive installation requires --model NAME|PATH, --model-url plus --model-sha256, or --skip-model")
	}
	reader := bufio.NewReader(i.in)
	recommended, recommendation := recommendModel(catalog, i.capacity, i.backend)
	fmt.Fprintln(i.out, "Choose a model:")
	if recommended != nil {
		fmt.Fprintf(i.out, "Press Enter for %s: %s.\n", recommended.Name, recommendation)
	}
	for n, m := range catalog {
		if m.URL == "" {
			fmt.Fprintf(i.out, "  %d) %s (resolved and pinned on selection)\n", n+1, m.Name)
		} else {
			fmt.Fprintf(i.out, "  %d) %s (%s)\n", n+1, m.Name, humanBytes(m.Size))
		}
	}
	localChoice := len(catalog) + 1
	customChoice := localChoice + 1
	skipChoice := customChoice + 1
	fmt.Fprintf(i.out, "  %d) Existing local GGUF\n  %d) HTTPS GGUF URL and trusted SHA256\n  %d) Install binaries only\nSelection: ", localChoice, customChoice, skipChoice)
	line, err := readLine(reader)
	if err != nil {
		return nil, "", err
	}
	if line == "" && recommended != nil {
		return recommended, "", nil
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > skipChoice {
		return nil, "", errors.New("invalid model selection")
	}
	if n <= len(catalog) {
		m := catalog[n-1]
		return &m, "", nil
	}
	if n == skipChoice {
		return nil, "", nil
	}
	if n == localChoice {
		fmt.Fprint(i.out, "GGUF path: ")
		p, err := readLine(reader)
		if err != nil {
			return nil, "", err
		}
		i.opts.model = p
		return i.selectModel(catalog)
	}
	fmt.Fprint(i.out, "HTTPS model URL: ")
	raw, err := readLine(reader)
	if err != nil {
		return nil, "", err
	}
	fmt.Fprint(i.out, "Trusted SHA256: ")
	hash, err := readLine(reader)
	if err != nil {
		return nil, "", err
	}
	i.opts.modelURL = raw
	i.opts.modelSHA = hash
	return i.selectModel(catalog)
}

func readLine(r *bufio.Reader) (string, error) {
	line, e := r.ReadString('\n')
	if e != nil && (e != io.EOF || strings.TrimSpace(line) == "") {
		return "", fmt.Errorf("reading selection: %w", e)
	}
	return strings.TrimSpace(line), nil
}

func humanBytes(n int64) string {
	if n <= 0 {
		return "size unspecified"
	}
	return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
}

func validRevision(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 20
}

func pinnedModelURL(repository, revision, filename string) string {
	u := url.URL{Scheme: "https", Host: "huggingface.co", Path: "/" + repository + "/resolve/" + revision + "/" + filename}
	return u.String()
}

// Resolve discovery metadata once and keep the exact immutable artifact pinned
// in the user's installation. Reruns reuse this record instead of moving to a
// changed upstream default branch. Only fully specified LFS records are usable.
func (i *installer) resolveSelectedModel(candidate model) (*model, error) {
	if candidate.URL != "" {
		if err := validateURL(candidate.URL); err != nil {
			return nil, err
		}
		if !validHash(candidate.SHA256) || candidate.Size < 0 || candidate.Size > maxDownloadBytes {
			return nil, errors.New("direct model pin requires a valid SHA256 and supported byte size")
		}
		if candidate.Repository != "" && (!validRevision(candidate.Revision) || candidate.Filename == "" || candidate.URL != pinnedModelURL(candidate.Repository, candidate.Revision, candidate.Filename)) {
			return nil, errors.New("catalog model provenance does not match its immutable download URL")
		}
		if err := i.persistModelPin(candidate); err != nil {
			return nil, err
		}
		return &candidate, nil
	}
	if candidate.Repository == "" || candidate.Filename == "" {
		return nil, errors.New("model selection has no download or discovery metadata")
	}
	manifest := filepath.Join(i.opts.prefix, "models", candidate.Name+".manifest.json")
	if data, err := os.ReadFile(manifest); err == nil {
		var pinned model
		if json.Unmarshal(data, &pinned) != nil || pinned.Name != candidate.Name || pinned.Repository != candidate.Repository || pinned.Filename != candidate.Filename || !validRevision(pinned.Revision) || !validHash(pinned.SHA256) || pinned.Size < 8 || pinned.Size > maxDownloadBytes || pinned.URL != pinnedModelURL(pinned.Repository, pinned.Revision, pinned.Filename) {
			return nil, fmt.Errorf("existing model pin is invalid; preserving %s; remove it explicitly to resolve trusted metadata again", manifest)
		}
		fmt.Fprintf(i.out, "Reusing pinned model revision %s\n", pinned.Revision)
		return &pinned, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	u := url.URL{Scheme: "https", Host: "huggingface.co", Path: "/api/models/" + candidate.Repository, RawQuery: "blobs=true"}
	fmt.Fprintf(i.out, "Resolving official model metadata for %s...\n", candidate.Repository)
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mini-Fabrics-Installer/1.0")
	metadataClient := *i.client
	metadataClient.Timeout = time.Minute
	metadataClient.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if err := secureRedirect(next, via); err != nil {
			return err
		}
		if next.URL.Hostname() != "huggingface.co" {
			return errors.New("official model metadata redirected away from huggingface.co")
		}
		return nil
	}
	resp, err := metadataClient.Do(req)
	if err != nil {
		var requestErr *url.Error
		if errors.As(err, &requestErr) {
			err = requestErr.Err
		}
		return nil, fmt.Errorf("reading official Hugging Face model metadata: %w; allow huggingface.co in network settings or choose a local GGUF", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("official Hugging Face model metadata returned HTTP %d; allow huggingface.co in network settings or choose a local GGUF", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 8<<20 {
		return nil, errors.New("official model metadata exceeds the response limit")
	}
	pinned, err := parseModelMetadata(candidate, data)
	if err != nil {
		return nil, fmt.Errorf("official model metadata is unusable: %w", err)
	}
	if err := i.persistModelPin(pinned); err != nil {
		return nil, err
	}
	fmt.Fprintf(i.out, "Pinned model revision %s; SHA256 %s (%s)\n", pinned.Revision, pinned.SHA256, humanBytes(pinned.Size))
	return &pinned, nil
}

func (i *installer) persistModelPin(pinned model) error {
	if pinned.Name == "" || strings.ContainsAny(pinned.Name, `/\:`) {
		return errors.New("invalid model pin name")
	}
	manifest := filepath.Join(i.opts.prefix, "models", pinned.Name+".manifest.json")
	if existing, err := os.ReadFile(manifest); err == nil {
		var previous model
		if json.Unmarshal(existing, &previous) != nil {
			return fmt.Errorf("existing model pin is invalid; preserving %s", manifest)
		}
		if previous == pinned {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	encoded, err := json.MarshalIndent(pinned, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(manifest), 0700); err != nil {
		return err
	}
	return atomicWrite(manifest, append(encoded, '\n'), 0600)
}

func parseModelMetadata(candidate model, data []byte) (model, error) {
	var metadata struct {
		ID       string `json:"id"`
		SHA      string `json:"sha"`
		Siblings []struct {
			Filename string `json:"rfilename"`
			Size     int64  `json:"size"`
			LFS      *struct {
				SHA256 string `json:"sha256"`
				Size   int64  `json:"size"`
			} `json:"lfs"`
		} `json:"siblings"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return model{}, err
	}
	if metadata.ID != candidate.Repository {
		return model{}, errors.New("repository identity does not match the selected official model")
	}
	if !validRevision(metadata.SHA) {
		return model{}, errors.New("missing immutable 40-character model revision")
	}
	found := false
	for _, file := range metadata.Siblings {
		if file.Filename != candidate.Filename {
			continue
		}
		if found {
			return model{}, errors.New("duplicate selected GGUF metadata records")
		}
		found = true
		if file.LFS == nil || !validHash(file.LFS.SHA256) || file.LFS.Size < 8 || file.LFS.Size > maxDownloadBytes || (file.Size != 0 && file.Size != file.LFS.Size) {
			return model{}, errors.New("selected GGUF lacks a valid LFS SHA256 and byte size")
		}
		candidate.SHA256 = strings.ToLower(file.LFS.SHA256)
		candidate.Size = file.LFS.Size
	}
	if !found {
		return model{}, fmt.Errorf("expected GGUF filename %s is missing", candidate.Filename)
	}
	candidate.Revision = strings.ToLower(metadata.SHA)
	candidate.URL = pinnedModelURL(candidate.Repository, candidate.Revision, candidate.Filename)
	return candidate, nil
}

func checkGGUF(file string) error {
	info, err := os.Stat(file)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("model must be a regular file")
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("model must be a regular file")
	}
	var head [24]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return err
	}
	version := binary.LittleEndian.Uint32(head[4:8])
	if string(head[:4]) != "GGUF" || (version != 2 && version != 3) {
		return errors.New("model does not have a supported GGUF v2/v3 header")
	}
	return nil
}

func (i *installer) install(plan backendPlan, selected *model, local string) error {
	prefix := i.opts.prefix
	if err := os.MkdirAll(prefix, 0700); err != nil {
		return err
	}
	// A single installer may write a prefix at a time. Never disturb a stale
	// lock automatically because another process may still own the build.
	lock := filepath.Join(prefix, ".install.lock")
	lf, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("acquiring install lock %s: %w; if no installer is running, remove the stale lock", lock, err)
	}
	fmt.Fprintf(lf, "pid=%d\n", os.Getpid())
	lf.Close()
	defer os.Remove(lock)
	cache := filepath.Join(prefix, "cache")
	if err := os.MkdirAll(cache, 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(prefix, ".install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	// Preserve preexisting configuration, including fields added by the runtime.
	config, err := readConfig(filepath.Join(prefix, "config.json"))
	if err != nil {
		return err
	}
	if local != "" {
		config["model_path"] = local
	}
	if selected != nil {
		selected, err = i.resolveSelectedModel(*selected)
		if err != nil {
			return err
		}
		if err := i.checkModelCapacity(*selected); err != nil {
			return err
		}
		modelPath, err := i.downloadModel(*selected)
		if err != nil {
			return err
		}
		config["model_path"] = modelPath
	}
	if i.native != nil {
		return i.installNative(stage, config, plan)
	}
	goPath, err := i.ensureGo(cache)
	if err != nil {
		return err
	}
	runtimeSource := i.opts.source
	if runtimeSource != "" {
		runtimeSource, err = filepath.Abs(runtimeSource)
		if err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(runtimeSource, "go.mod")); err != nil {
			return fmt.Errorf("--source must contain go.mod: %w", err)
		}
	} else {
		runtimeSource = filepath.Join(stage, "runtime")
		data, err := base64.StdEncoding.DecodeString(runtimeArchiveBase64)
		if err != nil {
			return err
		}
		if !validHash(runtimeArchiveSHA256) || hashBytes(data) != strings.ToLower(runtimeArchiveSHA256) {
			return errors.New("bundled runtime source failed SHA256 verification")
		}
		if err := extractTarGzip(bytes.NewReader(data), runtimeSource, true); err != nil {
			return fmt.Errorf("extracting bundled runtime: %w", err)
		}
	}
	stagedBin := filepath.Join(stage, "bin")
	if err := os.MkdirAll(stagedBin, 0700); err != nil {
		return err
	}
	runtimeBinary := filepath.Join(stagedBin, exeName("fabrics"))
	fmt.Fprintln(i.out, "Building the bundled Go runtime with locked dependency metadata...")
	// Respect explicitly configured caches (useful in managed development
	// environments). Otherwise keep every downloaded dependency under prefix.
	buildCache := os.Getenv("GOCACHE")
	if buildCache == "" {
		buildCache = filepath.Join(cache, "go-build")
	}
	moduleCache := os.Getenv("GOMODCACHE")
	if moduleCache == "" {
		moduleCache = filepath.Join(cache, "go-mod")
	}
	goEnv := append(os.Environ(), "CGO_ENABLED=0", "GOTOOLCHAIN=local", "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH, "GOCACHE="+buildCache, "GOMODCACHE="+moduleCache)
	if err := i.command(runtimeSource, goEnv, goPath, "build", "-mod=readonly", "-buildvcs=false", "-trimpath", "-o", runtimeBinary, "./cmd/fabrics"); err != nil {
		return err
	}
	if previous, ok := reusableLlama(prefix, plan.Name); ok {
		fmt.Fprintln(i.out, "Reusing the verified installed llama-server for this backend.")
		for name := range previous.Files {
			if err := copyFile(filepath.Join(prefix, "bin", name), filepath.Join(stagedBin, name), 0755); err != nil {
				return err
			}
		}
	} else if err := i.buildLlama(stage, stagedBin, cache, plan); err != nil {
		return err
	}
	if err := i.command("", nil, filepath.Join(stagedBin, exeName("llama-server")), "--version"); err != nil {
		return fmt.Errorf("validating built llama-server: %w", err)
	}
	if err := i.command("", nil, runtimeBinary, "--help"); err != nil {
		return fmt.Errorf("validating built runtime: %w", err)
	}
	binDir := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(binDir, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(stagedBin)
	if err != nil {
		return err
	}
	if _, ok := config["cognition_mode"]; !ok {
		config["cognition_mode"] = "adaptive"
	}
	config["llama_path"] = filepath.Join(binDir, exeName("llama-server"))
	applyBackendConfig(config, plan.Name)
	if _, ok := config["context_size"]; !ok {
		config["context_size"] = 8192
	}
	if _, ok := config["threads"]; !ok {
		config["threads"] = i.opts.jobs
	}
	if _, ok := config["port"]; !ok {
		config["port"] = 0
	}
	if _, ok := config["model_path"]; !ok {
		config["model_path"] = ""
	}
	if _, ok := config["system_prompt"]; !ok {
		config["system_prompt"] = "You are a helpful, thoughtful local assistant. Explain uncertainty clearly."
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	stagedConfig := filepath.Join(stage, "config.json")
	if err := atomicWrite(stagedConfig, append(encoded, '\n'), 0600); err != nil {
		return err
	}
	manifest := llamaManifest{SourceSHA256: llamaSHA256, BuildProfile: llamaBuildProfile, CPUIdentity: currentCPUIdentity(), CUDAIdentity: currentCUDAIdentity(plan.Name), Backend: plan.Name, Platform: runtime.GOOS + "/" + runtime.GOARCH, Files: make(map[string]string)}
	for _, e := range entries {
		if e.Name() != exeName("fabrics") && !e.IsDir() {
			hash, err := hashFile(filepath.Join(stagedBin, e.Name()))
			if err != nil {
				return err
			}
			manifest.Files[e.Name()] = hash
		}
	}
	metadata, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	stagedManifest := filepath.Join(stage, "llama-build.json")
	if err := atomicWrite(stagedManifest, append(metadata, '\n'), 0600); err != nil {
		return err
	}
	replacements := make([]fileReplacement, 0, len(entries)+2)
	for _, e := range entries {
		if !e.IsDir() {
			replacements = append(replacements, fileReplacement{filepath.Join(stagedBin, e.Name()), filepath.Join(binDir, e.Name())})
		}
	}
	replacements = append(replacements,
		fileReplacement{stagedConfig, filepath.Join(prefix, "config.json")},
		fileReplacement{stagedManifest, filepath.Join(prefix, "llama-build.json")})
	if err := publishFiles(replacements, i.out); err != nil {
		return fmt.Errorf("publishing installation: %w", err)
	}
	fmt.Fprintf(i.out, "Installed Mini Fabrics. Configuration: %s\nRun: %s --home %s doctor\n", filepath.Join(prefix, "config.json"), executableInvocation(filepath.Join(binDir, exeName("fabrics"))), shellQuote(prefix))
	if p, _ := config["model_path"].(string); p != "" {
		fmt.Fprintf(i.out, "Start: %s --home %s serve\n", executableInvocation(filepath.Join(binDir, exeName("fabrics"))), shellQuote(prefix))
	} else {
		fmt.Fprintln(i.out, "No model is configured. Rerun this installer with --model PATH or a verified model URL before starting inference.")
	}
	return nil
}

// Download separate content-addressed models before publishing config.json.
// A new revision must never replace bytes used by the active installation.
func (i *installer) downloadModel(selected model) (string, error) {
	modelPath := filepath.Join(i.opts.prefix, "models", selected.Name+"-"+strings.ToLower(selected.SHA256)+".gguf")
	if _, err := os.Stat(modelPath); errors.Is(err, os.ErrNotExist) {
		legacy := filepath.Join(i.opts.prefix, "models", selected.Name+".gguf")
		if hash, err := hashFile(legacy); err == nil && hash == strings.ToLower(selected.SHA256) {
			if err := copyFile(legacy, modelPath, 0600); err != nil {
				return "", fmt.Errorf("reusing verified model: %w", err)
			}
		}
	}
	fmt.Fprintf(i.out, "Preparing model %s; SHA256 %s\n", selected.Name, selected.SHA256)
	if err := i.download(selected.URL, modelPath, selected.SHA256, selected.Size); err != nil {
		return "", err
	}
	if err := checkGGUF(modelPath); err != nil {
		return "", fmt.Errorf("downloaded model: %w", err)
	}
	return modelPath, nil
}

func applyBackendConfig(config map[string]any, backend string) {
	previous, _ := config["backend"].(string)
	// Preserve deliberate VRAM tuning when reinstalling the same backend.
	// A backend change needs a fresh backend-appropriate default.
	if _, set := config["gpu_layers"]; !set || previous != backend {
		layers := 0
		if backend != "cpu" {
			layers = -1
		}
		config["gpu_layers"] = layers
	}
	config["backend"] = backend
}

type fileReplacement struct{ source, destination string }

// Stage all build/configuration work before calling this helper. Retain every
// old file until the complete set has been installed, then restore the set if
// any ordinary filesystem operation fails. Backups stay beside destinations
// so they survive stage cleanup if a rollback itself cannot complete. This is
// rollback on reported failures, not a power-loss-safe filesystem transaction.
func publishFiles(files []fileReplacement, out io.Writer) error {
	type replacementState struct {
		fileReplacement
		backup    string
		published bool
	}
	var changed []replacementState
	rollback := func(cause error) error {
		for n := len(changed) - 1; n >= 0; n-- {
			item := changed[n]
			if item.published {
				if err := os.Remove(item.destination); err != nil {
					cause = errors.Join(cause, fmt.Errorf("removing replacement %s: %w; previous file remains at %s", item.destination, err, item.backup))
					continue
				}
			}
			if item.backup != "" {
				if err := os.Rename(item.backup, item.destination); err != nil {
					cause = errors.Join(cause, fmt.Errorf("restoring %s: %w; previous file remains at %s", item.destination, err, item.backup))
				}
			}
		}
		return cause
	}
	for _, file := range files {
		info, err := os.Stat(file.source)
		if err != nil {
			return rollback(err)
		}
		if !info.Mode().IsRegular() {
			return rollback(fmt.Errorf("staged file %s is not regular", file.source))
		}
		item := replacementState{fileReplacement: file}
		if info, err := os.Lstat(file.destination); err == nil {
			if !info.Mode().IsRegular() {
				return rollback(fmt.Errorf("preserving non-regular destination %s", file.destination))
			}
			backup, err := os.CreateTemp(filepath.Dir(file.destination), ".previous-")
			if err != nil {
				return rollback(err)
			}
			item.backup = backup.Name()
			if err := backup.Close(); err != nil {
				os.Remove(item.backup)
				return rollback(err)
			}
			if err := os.Remove(item.backup); err != nil {
				return rollback(err)
			}
			if err := os.Rename(file.destination, item.backup); err != nil {
				return rollback(err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return rollback(err)
		}
		changed = append(changed, item)
		if err := os.Rename(file.source, file.destination); err != nil {
			return rollback(err)
		}
		changed[len(changed)-1].published = true
	}
	for _, item := range changed {
		if item.backup != "" {
			if err := os.Remove(item.backup); err != nil {
				fmt.Fprintf(out, "Installed successfully; could not remove old backup %s: %v\n", item.backup, err)
			}
		}
	}
	return nil
}

type llamaManifest struct {
	SourceSHA256 string            `json:"source_sha256"`
	BuildProfile string            `json:"build_profile"`
	CPUIdentity  string            `json:"cpu_identity"`
	CUDAIdentity string            `json:"cuda_identity,omitempty"`
	Backend      string            `json:"backend"`
	Platform     string            `json:"platform"`
	Files        map[string]string `json:"files"`
}

// GGML_NATIVE can compile CUDA only for the installed devices. Reusing that
// binary after replacing a GPU or changing its driver must require a rebuild.
func currentCUDAIdentity(backend string) string {
	if backend != "cuda" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=name,compute_cap,driver_version", "--format=csv,noheader").Output()
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return ""
	}
	visible, set := os.LookupEnv("CUDA_VISIBLE_DEVICES")
	identity := string(bytes.TrimSpace(data)) + "\nCUDA_VISIBLE_DEVICES=" + strconv.FormatBool(set) + ":" + visible + "\nCUDA_DEVICE_ORDER=" + os.Getenv("CUDA_DEVICE_ORDER")
	return hashBytes([]byte(identity))
}

// A native build must never be reused merely because an installation directory
// moved to another machine with the same OS/architecture. Include CPU features
// where the platform exposes them; missing identity disables reuse safely.
func currentCPUIdentity() string {
	var identity string
	switch runtime.GOOS {
	case "linux":
		data, err := os.ReadFile("/proc/cpuinfo")
		if err != nil {
			return ""
		}
		first, _, _ := strings.Cut(string(data), "\n\n")
		for _, line := range strings.Split(first, "\n") {
			key, value, found := strings.Cut(line, ":")
			if !found {
				continue
			}
			switch strings.TrimSpace(key) {
			case "vendor_id", "model name", "cpu family", "model", "stepping", "flags", "Features", "CPU implementer", "CPU architecture", "CPU variant", "CPU part", "CPU revision":
				identity += strings.TrimSpace(key) + ":" + strings.Join(strings.Fields(value), " ") + "\n"
			}
		}
	case "darwin":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		args := []string{"-n", "machdep.cpu.brand_string", "machdep.cpu.features", "machdep.cpu.leaf7_features"}
		if runtime.GOARCH == "arm64" {
			args = []string{"-n", "hw.model", "hw.cpufamily", "hw.cputype"}
		}
		data, err := exec.CommandContext(ctx, "sysctl", args...).Output()
		if err != nil {
			return ""
		}
		identity = strings.TrimSpace(string(data))
	case "windows":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		data, err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "Get-CimInstance Win32_Processor | Select-Object Name,Manufacturer,Revision,Architecture,ProcessorId | ConvertTo-Json -Compress").Output()
		if err != nil {
			return ""
		}
		identity = strings.TrimSpace(string(data))
	}
	if identity == "" {
		return ""
	}
	return hashBytes([]byte(runtime.GOOS + "/" + runtime.GOARCH + "\n" + identity))
}

func reusableLlama(prefix, backend string) (llamaManifest, bool) {
	var manifest llamaManifest
	data, err := os.ReadFile(filepath.Join(prefix, "llama-build.json"))
	if err != nil || json.Unmarshal(data, &manifest) != nil {
		return manifest, false
	}
	cpuIdentity := currentCPUIdentity()
	if cpuIdentity == "" || manifest.CPUIdentity != cpuIdentity || manifest.SourceSHA256 != llamaSHA256 || manifest.BuildProfile != llamaBuildProfile || manifest.Backend != backend || manifest.Platform != runtime.GOOS+"/"+runtime.GOARCH || len(manifest.Files) == 0 || len(manifest.Files) > 100 {
		return manifest, false
	}
	if backend == "cuda" {
		identity := currentCUDAIdentity(backend)
		if identity == "" || manifest.CUDAIdentity != identity {
			return manifest, false
		}
	}
	if _, ok := manifest.Files[exeName("llama-server")]; !ok {
		return manifest, false
	}
	for name, expected := range manifest.Files {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\\:`) || name == exeName("fabrics") || !validHash(expected) {
			return manifest, false
		}
		actual, err := hashFile(filepath.Join(prefix, "bin", name))
		if err != nil || actual != expected {
			return manifest, false
		}
	}
	return manifest, true
}

func (i *installer) buildLlama(stage, stagedBin, cache string, plan backendPlan) error {
	archive := filepath.Join(cache, "llama-"+llamaCommit+".tar.gz")
	fmt.Fprintln(i.out, "Downloading and verifying pinned llama.cpp source...")
	if err := i.download(llamaURL, archive, llamaSHA256, 0); err != nil {
		return err
	}
	src := filepath.Join(stage, "llama")
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	err = extractTarGzip(f, src, true)
	f.Close()
	if err != nil {
		return err
	}
	buildDir := filepath.Join(stage, "llama-build")
	cmakeArgs := []string{"-S", src, "-B", buildDir, "-DCMAKE_BUILD_TYPE=Release", "-DBUILD_SHARED_LIBS=OFF", "-DGGML_STATIC=OFF", "-DLLAMA_BUILD_SERVER=ON", "-DLLAMA_BUILD_TESTS=OFF", "-DLLAMA_BUILD_EXAMPLES=ON", "-DLLAMA_OPENSSL=OFF", "-DLLAMA_BUILD_UI=OFF", "-DLLAMA_USE_PREBUILT_UI=OFF", "-DLLAMA_SUBPROCESS=OFF", "-DGGML_NATIVE=ON", "-DGGML_CUDA=OFF", "-DGGML_METAL=OFF", "-DGGML_VULKAN=OFF"}
	if runtime.GOOS == "windows" {
		if _, e := i.lookup("ninja"); e == nil {
			cmakeArgs = append(cmakeArgs, "-G", "Ninja")
		}
	}
	switch plan.Name {
	case "cuda":
		cmakeArgs = append(cmakeArgs, "-DGGML_CUDA=ON")
	case "metal":
		cmakeArgs = append(cmakeArgs, "-DGGML_METAL=ON", "-DGGML_METAL_EMBED_LIBRARY=ON")
	case "vulkan":
		cmakeArgs = append(cmakeArgs, "-DGGML_VULKAN=ON")
	}
	fmt.Fprintf(i.out, "Compiling llama-server (%s, %d jobs)...\n", plan.Name, i.opts.jobs)
	if err := i.command("", nil, "cmake", cmakeArgs...); err != nil {
		return err
	}
	if err := i.command("", nil, "cmake", "--build", buildDir, "--config", "Release", "--target", "llama-server", "--parallel", strconv.Itoa(i.opts.jobs)); err != nil {
		return err
	}
	llamaBuilt := filepath.Join(buildDir, "bin", exeName("llama-server"))
	if _, e := os.Stat(llamaBuilt); e != nil {
		llamaBuilt = filepath.Join(buildDir, "bin", "Release", exeName("llama-server"))
	}
	if err := copyFile(llamaBuilt, filepath.Join(stagedBin, exeName("llama-server")), 0755); err != nil {
		return err
	}
	// A static llama build still needs platform runtime libraries on Windows.
	// Copy CMake-produced DLLs next to the executable, never arbitrary PATH DLLs.
	if runtime.GOOS == "windows" {
		if err := filepath.WalkDir(filepath.Dir(llamaBuilt), func(p string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".dll") {
				return copyFile(p, filepath.Join(stagedBin, filepath.Base(p)), 0755)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func readConfig(file string) (map[string]any, error) {
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c map[string]any
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("existing configuration is invalid; preserving %s: %w", file, err)
	}
	if c == nil {
		return nil, errors.New("existing configuration must be a JSON object")
	}
	return c, nil
}

func (i *installer) ensureGo(cache string) (string, error) {
	if p, e := i.lookup("go"); e == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		b, e := exec.CommandContext(ctx, p, "version").Output()
		if e == nil && sufficientGoVersion(string(b)) {
			return p, nil
		}
		fmt.Fprintln(i.out, "Installed Go is unavailable or older than 1.24; bootstrapping the pinned toolchain under the installation directory.")
	}
	key := runtime.GOOS + "-" + runtime.GOARCH
	hash, ok := goArchiveHashes[key]
	if !ok {
		return "", fmt.Errorf("Go bootstrap does not support %s; install Go >=1.24 and rerun", key)
	}
	toolchain := filepath.Join(cache, "go"+goVersion+"-"+key)
	goPath := filepath.Join(toolchain, "bin", exeName("go"))
	if _, e := os.Stat(goPath); e == nil {
		return goPath, nil
	}
	ext := ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	name := "go" + goVersion + "." + key + ext
	archive := filepath.Join(cache, name)
	if err := i.download("https://go.dev/dl/"+name, archive, hash, 0); err != nil {
		return "", err
	}
	stage, e := os.MkdirTemp(cache, ".go-")
	if e != nil {
		return "", e
	}
	defer os.RemoveAll(stage)
	if ext == ".zip" {
		e = extractZip(archive, stage, true)
	} else {
		var f *os.File
		f, e = os.Open(archive)
		if e == nil {
			e = extractTarGzip(f, stage, true)
			f.Close()
		}
	}
	if e != nil {
		return "", fmt.Errorf("extracting verified Go toolchain: %w", e)
	}
	if err := os.Rename(stage, toolchain); err != nil {
		return "", err
	}
	return goPath, nil
}

func sufficientGoVersion(s string) bool {
	for _, f := range strings.Fields(s) {
		if strings.HasPrefix(f, "go1.") {
			parts := strings.Split(strings.TrimPrefix(f, "go"), ".")
			if len(parts) >= 2 {
				minor, e := strconv.Atoi(parts[1])
				return e == nil && minor >= 24
			}
		}
	}
	return false
}

func (i *installer) command(dir string, env []string, name string, args ...string) error {
	fmt.Fprintf(i.out, "Running %s\n", filepath.Base(name))
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = i.out
	cmd.Stderr = i.out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", filepath.Base(name), err)
	}
	return nil
}

func (i *installer) download(raw, dest, expected string, size int64) error {
	if err := validateURL(raw); err != nil {
		return err
	}
	if !validHash(expected) {
		return errors.New("artifact has no valid pinned SHA256")
	}
	if size < 0 || size > maxDownloadBytes {
		return errors.New("artifact exceeds the 64 GiB download limit")
	}
	expected = strings.ToLower(expected)
	if got, e := hashFile(dest); e == nil && got == expected {
		if size <= 0 {
			return nil
		}
		if st, e := os.Stat(dest); e == nil && st.Size() == size {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dest), ".download-")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	req, err := http.NewRequest("GET", raw, nil)
	if err != nil {
		f.Close()
		return err
	}
	req.Header.Set("User-Agent", "Mini-Fabrics-Installer/1.0")
	resp, err := i.client.Do(req)
	if err != nil {
		f.Close()
		// Redirect URLs may carry signed download credentials. Report the host
		// and underlying transport error without printing their query strings.
		var requestErr *url.Error
		failedHost := req.URL.Hostname()
		if errors.As(err, &requestErr) {
			if failedURL, parseErr := url.Parse(requestErr.URL); parseErr == nil && failedURL.Hostname() != "" {
				failedHost = failedURL.Hostname()
			}
			err = requestErr.Err
		}
		return fmt.Errorf("downloading from %s: %w", failedHost, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		f.Close()
		return fmt.Errorf("download from %s returned HTTP %d; check network settings for the host and HTTPS redirects", req.URL.Hostname(), resp.StatusCode)
	}
	h := sha256.New()
	reader := io.Reader(io.LimitReader(resp.Body, maxDownloadBytes+1))
	if size > 0 {
		reader = io.LimitReader(resp.Body, size+1)
	}
	n, err := io.Copy(io.MultiWriter(f, h), reader)
	if err != nil {
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
	if n > maxDownloadBytes {
		return errors.New("artifact exceeds the 64 GiB download limit; previous files preserved")
	}
	if size > 0 && n != size {
		return fmt.Errorf("artifact size mismatch: received %d bytes, expected %d", n, size)
	}
	if hex.EncodeToString(h.Sum(nil)) != expected {
		return fmt.Errorf("SHA256 verification failed for artifact from %s; previous files preserved", req.URL.Hostname())
	}
	return replaceFile(temp, dest)
}

func hashBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func hashFile(file string) (string, error) {
	info, err := os.Stat(file)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("artifact must be a regular file")
	}
	f, e := os.Open(file)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func safeArchivePath(root, name string, strip bool) (string, error) {
	if strings.ContainsAny(name, "\\:") || strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) {
		return "", errors.New("unsafe archive path")
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("archive path escapes installation directory")
	}
	if strip {
		parts := strings.SplitN(clean, "/", 2)
		if len(parts) == 1 {
			return "", nil
		}
		clean = parts[1]
	}
	if clean == "." || clean == "" {
		return "", nil
	}
	full := filepath.Join(root, filepath.FromSlash(clean))
	rel, e := filepath.Rel(root, full)
	if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("archive path escapes installation directory")
	}
	return full, nil
}

func extractTarGzip(r io.Reader, dest string, strip bool) error {
	if err := os.MkdirAll(dest, 0700); err != nil {
		return err
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for {
		h, e := tr.Next()
		if e == io.EOF {
			return nil
		}
		if e != nil {
			return e
		}
		p, e := safeArchivePath(dest, h.Name, strip)
		if e != nil {
			return e
		}
		if p == "" {
			continue
		}
		if h.Size < 0 || h.Size > maxArchiveBytes-total {
			return errors.New("archive exceeds extraction size limit")
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if e = os.MkdirAll(p, 0700); e != nil {
				return e
			}
		case tar.TypeReg, tar.TypeRegA:
			total += h.Size
			if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
				return e
			}
			mode := os.FileMode(0600)
			if h.Mode&0111 != 0 {
				mode = 0700
			}
			f, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if e != nil {
				return e
			}
			_, e = io.CopyN(f, tr, h.Size)
			closeErr := f.Close()
			if e != nil {
				return e
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("unsupported archive entry type %d (links and special files are rejected)", h.Typeflag)
		}
	}
}

func extractZip(file, dest string, strip bool) error {
	z, err := zip.OpenReader(file)
	if err != nil {
		return err
	}
	defer z.Close()
	var total uint64
	for _, entry := range z.File {
		p, e := safeArchivePath(dest, entry.Name, strip)
		if e != nil {
			return e
		}
		if p == "" {
			continue
		}
		if entry.Mode()&os.ModeSymlink != 0 {
			return errors.New("archive symlinks are rejected")
		}
		if entry.UncompressedSize64 > uint64(maxArchiveBytes)-total {
			return errors.New("archive exceeds extraction size limit")
		}
		if entry.FileInfo().IsDir() {
			if e = os.MkdirAll(p, 0700); e != nil {
				return e
			}
			continue
		}
		total += entry.UncompressedSize64
		if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			return e
		}
		r, e := entry.Open()
		if e != nil {
			return e
		}
		mode := os.FileMode(0600)
		if entry.Mode()&0111 != 0 {
			mode = 0700
		}
		f, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if e != nil {
			r.Close()
			return e
		}
		n, e := io.Copy(f, io.LimitReader(r, int64(entry.UncompressedSize64)+1))
		r.Close()
		closeErr := f.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
		if uint64(n) != entry.UncompressedSize64 {
			return errors.New("zip entry size mismatch")
		}
	}
	return nil
}

func exeName(s string) string {
	if runtime.GOOS == "windows" {
		return s + ".exe"
	}
	return s
}

func copyFile(src, dst string, mode os.FileMode) error {
	r, e := os.Open(src)
	if e != nil {
		return e
	}
	defer r.Close()
	w, e := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if e != nil {
		return e
	}
	_, e = io.Copy(w, r)
	closeErr := w.Close()
	if e != nil {
		return e
	}
	return closeErr
}

// Rename on Windows cannot replace an existing file. Keep a backup until the
// replacement succeeds so an unsuccessful update preserves its previous file.
func replaceFile(src, dst string) error {
	if _, e := os.Lstat(dst); errors.Is(e, os.ErrNotExist) {
		return os.Rename(src, dst)
	} else if e != nil {
		return e
	}
	backup, e := os.CreateTemp(filepath.Dir(dst), ".previous-")
	if e != nil {
		return e
	}
	name := backup.Name()
	backup.Close()
	os.Remove(name)
	if e = os.Rename(dst, name); e != nil {
		return e
	}
	if e = os.Rename(src, dst); e != nil {
		restoreErr := os.Rename(name, dst)
		if restoreErr != nil {
			return fmt.Errorf("replacement failed: %w; previous file remains at %s (restore failed: %v)", e, name, restoreErr)
		}
		return e
	}
	return os.Remove(name)
}

func atomicWrite(dst string, b []byte, mode os.FileMode) error {
	f, e := os.CreateTemp(filepath.Dir(dst), ".config-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(mode); e != nil {
		f.Close()
		return e
	}
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return replaceFile(name, dst)
}

func shellQuote(s string) string {
	if runtime.GOOS == "windows" {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func executableInvocation(s string) string {
	if runtime.GOOS == "windows" {
		return "& " + shellQuote(s)
	}
	return shellQuote(s)
}

// Native releases carry their complete, platform-specific runtime. Compilers
// and package registries are needed only when source mode is explicitly used.
type nativeManifest struct {
	FormatVersion  int               `json:"format_version"`
	Version        string            `json:"version"`
	Platform       string            `json:"platform"`
	Backends       []string          `json:"backends"`
	DefaultBackend string            `json:"default_backend"`
	BackendPaths   map[string]string `json:"backend_paths"`
	LlamaCommit    string            `json:"llama_commit"`
	Files          map[string]string `json:"files"`
	Raw            json.RawMessage   `json:"-"`
}

func loadNativeManifest(encoded, goos, goarch string) (*nativeManifest, error) {
	if encoded == "" {
		return nil, nil
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decoding native manifest: %w", err)
	}
	var manifest nativeManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	manifest.Raw = append(json.RawMessage(nil), data...)
	if manifest.FormatVersion != 1 || manifest.Version == "" || manifest.Platform != goos+"/"+goarch || manifest.LlamaCommit != llamaCommit || len(manifest.Files) == 0 || len(manifest.Files) > 4096 {
		return nil, errors.New("native release manifest has an unsupported version, platform, source pin, or file count")
	}
	seen := make(map[string]bool)
	licenses := false
	for name, hash := range manifest.Files {
		if path.Clean(name) != name || strings.ContainsAny(name, `\:`) || strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) || !validHash(hash) || seen[strings.ToLower(name)] {
			return nil, fmt.Errorf("invalid native release file %q", name)
		}
		if !strings.HasPrefix(name, "bin/") && !strings.HasPrefix(name, "backends/") && !strings.HasPrefix(name, "licenses/") {
			return nil, fmt.Errorf("unsupported native release path %q", name)
		}
		seen[strings.ToLower(name)] = true
		licenses = licenses || strings.HasPrefix(name, "licenses/")
	}
	exe := "fabrics"
	if goos == "windows" {
		exe += ".exe"
	}
	if _, ok := manifest.Files["bin/"+exe]; !ok || !licenses {
		return nil, errors.New("native release must contain the runtime executable and licenses")
	}
	available := make(map[string]bool)
	for _, backend := range manifest.Backends {
		if available[backend] || (backend != "cpu" && backend != "cuda" && backend != "metal" && backend != "vulkan") {
			return nil, errors.New("invalid native backend list")
		}
		available[backend] = true
		p := manifest.BackendPaths[backend]
		if _, ok := manifest.Files[p]; !ok || !strings.HasPrefix(p, "backends/"+backend+"/") {
			return nil, fmt.Errorf("native %s backend executable is missing", backend)
		}
	}
	if len(manifest.BackendPaths) != len(available) || !available["cpu"] || !available[manifest.DefaultBackend] {
		return nil, errors.New("native release requires CPU fallback and a supported default backend")
	}
	return &manifest, nil
}

func extractNativePayload(encoded, expected, dest string, manifest *nativeManifest) error {
	if !validHash(expected) {
		return errors.New("native runtime archive has no valid pinned SHA256")
	}
	checksum := sha256.New()
	n, err := io.Copy(checksum, io.LimitReader(base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded)), maxArchiveBytes+1))
	if err != nil || n > maxArchiveBytes || hex.EncodeToString(checksum.Sum(nil)) != strings.ToLower(expected) {
		return errors.New("native runtime archive failed SHA256 verification")
	}
	// Decode again for extraction after verification. Streaming avoids a second
	// archive-sized allocation for native CUDA bundles on smaller machines.
	if err := extractTarGzip(base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded)), dest, true); err != nil {
		return fmt.Errorf("extracting native runtime: %w", err)
	}
	found := make(map[string]bool)
	err = filepath.WalkDir(dest, func(file string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dest, file)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		expected, ok := manifest.Files[name]
		if !ok || !entry.Type().IsRegular() {
			return fmt.Errorf("unlisted or non-regular native file %q", name)
		}
		actual, err := hashFile(file)
		if err != nil || actual != strings.ToLower(expected) {
			return fmt.Errorf("native file %q failed verification", name)
		}
		found[name] = true
		return nil
	})
	if err != nil {
		return err
	}
	if len(found) != len(manifest.Files) {
		return errors.New("native archive is missing manifest files")
	}
	return nil
}

func chooseNativeBackend(request string, h hardware, manifest *nativeManifest) (backendPlan, error) {
	available := func(name string) bool { _, ok := manifest.BackendPaths[name]; return ok }
	if request == "auto" {
		if h.Metal && available("metal") {
			return backendPlan{"metal", "bundled Metal backend on supported macOS"}, nil
		}
		if h.NVIDIA && available("cuda") {
			return backendPlan{"cuda", "bundled CUDA backend and working NVIDIA driver"}, nil
		}
		if h.NVIDIA && !available("cuda") {
			return backendPlan{"cpu", "NVIDIA GPU detected; this release has no CUDA variant, so CPU is selected"}, nil
		}
		if h.Metal && !available("metal") {
			return backendPlan{"cpu", "Metal detected; this release has no Metal variant, so CPU is selected"}, nil
		}
		return backendPlan{"cpu", "bundled CPU backend; no matching usable GPU variant detected"}, nil
	}
	if !available(request) {
		return backendPlan{}, fmt.Errorf("native release has no %s backend; select a bundled backend or use --build-from-source", request)
	}
	if request == "cuda" && !h.NVIDIA {
		return backendPlan{}, errors.New("bundled CUDA backend requires a working NVIDIA driver")
	}
	if request == "metal" && h.OS != "darwin" {
		return backendPlan{}, errors.New("Metal requires macOS")
	}
	return backendPlan{request, "explicitly selected bundled backend"}, nil
}

func nativeCommandEnv(executable string) []string {
	dir := filepath.Dir(executable)
	key := "LD_LIBRARY_PATH"
	if runtime.GOOS == "windows" {
		key = "PATH"
	} else if runtime.GOOS != "linux" {
		return os.Environ()
	}
	value := dir
	if previous := os.Getenv(key); previous != "" {
		value += string(os.PathListSeparator) + previous
	}
	return append(os.Environ(), key+"="+value)
}

func (i *installer) installNative(stage string, config map[string]any, plan backendPlan) error {
	staged := filepath.Join(stage, "native")
	var replacements []fileReplacement
	for name := range i.native.Files {
		source := filepath.Join(i.nativeRoot, filepath.FromSlash(name))
		local := filepath.Join(staged, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(local), 0700); err != nil {
			return err
		}
		info, err := os.Stat(source)
		if err != nil {
			return err
		}
		if err := copyFile(source, local, info.Mode().Perm()); err != nil {
			return err
		}
		dest := filepath.Join(i.opts.prefix, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return err
		}
		replacements = append(replacements, fileReplacement{local, dest})
	}
	llamaRelative := filepath.FromSlash(i.native.BackendPaths[plan.Name])
	llamaStaged := filepath.Join(staged, llamaRelative)
	if err := i.command("", nativeCommandEnv(llamaStaged), llamaStaged, "--version"); err != nil {
		return fmt.Errorf("native llama-server cannot run on this host: %w", err)
	}
	fabricsStaged := filepath.Join(staged, "bin", exeName("fabrics"))
	if err := i.command("", nativeCommandEnv(fabricsStaged), fabricsStaged, "--help"); err != nil {
		return fmt.Errorf("native runtime cannot run on this host: %w", err)
	}
	config["llama_path"] = filepath.Join(i.opts.prefix, llamaRelative)
	applyBackendConfig(config, plan.Name)
	defaults := map[string]any{"context_size": 8192, "threads": i.opts.jobs, "port": 0, "model_path": "", "system_prompt": "You are a helpful, thoughtful local assistant. Explain uncertainty clearly.", "cognition_mode": "adaptive"}
	for key, value := range defaults {
		if _, ok := config[key]; !ok {
			config[key] = value
		}
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	configPath := filepath.Join(stage, "config.json")
	if err := atomicWrite(configPath, append(encoded, '\n'), 0600); err != nil {
		return err
	}
	manifest := []byte(i.native.Raw)
	if len(manifest) == 0 {
		manifest, err = json.MarshalIndent(i.native, "", "  ")
		if err != nil {
			return err
		}
	}
	manifestPath := filepath.Join(stage, "native-release.json")
	if err := atomicWrite(manifestPath, append(manifest, '\n'), 0600); err != nil {
		return err
	}
	replacements = append(replacements, fileReplacement{configPath, filepath.Join(i.opts.prefix, "config.json")}, fileReplacement{manifestPath, filepath.Join(i.opts.prefix, "native-release.json")})
	if err := publishFiles(replacements, i.out); err != nil {
		return err
	}
	fmt.Fprintf(i.out, "Installed Mini Fabrics %s native runtime (%s). No compiler or build-tool downloads were needed.\nRun: %s --home %s doctor\n", i.native.Version, plan.Name, executableInvocation(filepath.Join(i.opts.prefix, "bin", exeName("fabrics"))), shellQuote(i.opts.prefix))
	return nil
}

type memoryCapacity struct {
	RAMTotal      int64  `json:"ram_total_bytes"`
	RAMAvailable  int64  `json:"ram_available_bytes"`
	VRAMTotal     int64  `json:"vram_total_bytes"`
	VRAMAvailable int64  `json:"vram_available_bytes"`
	Unified       bool   `json:"unified_memory"`
	Metal         bool   `json:"metal_available"`
	RAMKnown      bool   `json:"-"`
	VRAMKnown     bool   `json:"-"`
	Source        string `json:"-"`
}

func commandOutput(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

func parseLinuxMemory(data string) memoryCapacity {
	var c memoryCapacity
	for _, line := range strings.Split(data, "\n") {
		key, value, ok := strings.Cut(line, ":")
		fields := strings.Fields(value)
		if !ok || len(fields) != 2 || fields[1] != "kB" {
			continue
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || n < 0 || n > (1<<60)/1024 {
			continue
		}
		switch key {
		case "MemTotal":
			c.RAMTotal = n * 1024
		case "MemAvailable":
			c.RAMAvailable = n * 1024
			c.RAMKnown = true
		}
	}
	c.Source = "Linux MemAvailable (including reclaimable memory)"
	return c
}

func restrictMemory(c memoryCapacity, limit, used int64) memoryCapacity {
	if limit <= 0 || used < 0 {
		return c
	}
	if c.RAMTotal == 0 || limit < c.RAMTotal {
		c.RAMTotal = limit
	}
	available := limit - used
	if available < 0 {
		available = 0
	}
	if !c.RAMKnown || available < c.RAMAvailable {
		c.RAMAvailable = available
		c.RAMKnown = true
	}
	return c
}

func readCapacityJSON(data []byte) (memoryCapacity, error) {
	var c memoryCapacity
	if err := json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return c, err
	}
	if c.RAMTotal < 0 || c.RAMAvailable < 0 || c.VRAMTotal < 0 || c.VRAMAvailable < 0 || (c.RAMTotal > 0 && c.RAMAvailable > c.RAMTotal) || (c.VRAMTotal > 0 && c.VRAMAvailable > c.VRAMTotal) {
		return c, errors.New("invalid memory capacity values")
	}
	_, c.RAMKnown = fields["ram_available_bytes"]
	_, c.VRAMKnown = fields["vram_available_bytes"]
	c.RAMKnown = c.RAMKnown && c.RAMTotal > 0
	c.VRAMKnown = c.VRAMKnown && c.VRAMTotal > 0
	return c, nil
}

func detectCapacity(h hardware, helper string) memoryCapacity {
	var c memoryCapacity
	switch h.OS {
	case "linux":
		if data, err := os.ReadFile("/proc/meminfo"); err == nil {
			c = parseLinuxMemory(string(data))
		}
		for _, pair := range [][2]string{{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory.current"}, {"/sys/fs/cgroup/memory/memory.limit_in_bytes", "/sys/fs/cgroup/memory/memory.usage_in_bytes"}} {
			limitData, err := os.ReadFile(pair[0])
			if err != nil {
				continue
			}
			usedData, err := os.ReadFile(pair[1])
			if err != nil {
				continue
			}
			limit, err := strconv.ParseInt(strings.TrimSpace(string(limitData)), 10, 64)
			if err != nil {
				continue
			}
			used, err := strconv.ParseInt(strings.TrimSpace(string(usedData)), 10, 64)
			if err != nil {
				continue
			}
			c = restrictMemory(c, limit, used)
		}
	case "windows":
		data, err := commandOutput("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "$o=Get-CimInstance Win32_OperatingSystem; @{ram_total_bytes=([int64]$o.TotalVisibleMemorySize*1024);ram_available_bytes=([int64]$o.FreePhysicalMemory*1024)} | ConvertTo-Json -Compress")
		if err == nil {
			if parsed, err := readCapacityJSON(data); err == nil {
				c = parsed
				c.Source = "Windows available physical memory"
			}
		}
	case "darwin":
		if helper != "" {
			if data, err := commandOutput(helper); err == nil {
				if parsed, err := readCapacityJSON(data); err == nil && parsed.RAMKnown {
					c = parsed
					c.Source = "native Swift/Mach memory and Metal working-set budget"
					break
				}
			}
		}
		c.Unified = h.Arch == "arm64"
		if data, err := commandOutput("sysctl", "-n", "hw.memsize"); err == nil {
			c.RAMTotal, _ = strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		}
		if data, err := commandOutput("vm_stat"); err == nil {
			var pageSize, pages int64
			var free, inactive bool
			for _, line := range strings.Split(string(data), "\n") {
				fields := strings.Fields(line)
				if strings.HasPrefix(line, "Mach Virtual Memory Statistics") {
					for n := 0; n+1 < len(fields); n++ {
						if fields[n] == "of" {
							pageSize, _ = strconv.ParseInt(fields[n+1], 10, 64)
						}
					}
				}
				if strings.HasPrefix(line, "Pages free:") || strings.HasPrefix(line, "Pages inactive:") {
					if len(fields) > 0 {
						value, err := strconv.ParseInt(strings.TrimSuffix(fields[len(fields)-1], "."), 10, 64)
						if err == nil && value >= 0 {
							pages += value
							free = free || strings.HasPrefix(line, "Pages free:")
							inactive = inactive || strings.HasPrefix(line, "Pages inactive:")
						}
					}
				}
			}
			if free && inactive && pageSize > 0 && pages <= (1<<60)/pageSize {
				c.RAMAvailable = pages * pageSize
				c.RAMKnown = true
			}
		}
		c.Source = "macOS sysctl/vm_stat estimate; Metal budget unknown"
	}
	if h.NVIDIA {
		if data, err := commandOutput("nvidia-smi", "--query-gpu=memory.total,memory.free", "--format=csv,noheader,nounits"); err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				values := strings.Split(line, ",")
				if len(values) != 2 {
					continue
				}
				total, e1 := strconv.ParseInt(strings.TrimSpace(values[0]), 10, 64)
				free, e2 := strconv.ParseInt(strings.TrimSpace(values[1]), 10, 64)
				if e1 == nil && e2 == nil && total > 0 && free >= 0 && free <= total && total < (1<<60)/(1<<20) && (!c.VRAMKnown || free*(1<<20) > c.VRAMAvailable) {
					c.VRAMTotal, c.VRAMAvailable, c.VRAMKnown = total*(1<<20), free*(1<<20), true
				}
			}
		}
	}
	return c
}

func capacityBudget(c memoryCapacity, backend string) (int64, bool, string) {
	if backend == "metal" && c.Unified {
		if !c.RAMKnown {
			return 0, false, "available unified memory is unknown"
		}
		budget := c.RAMAvailable
		if c.VRAMKnown && c.VRAMAvailable < budget {
			budget = c.VRAMAvailable
		}
		return budget, true, "available unified RAM capped by the Metal working-set budget"
	}
	if backend == "cuda" || backend == "vulkan" || backend == "metal" {
		if !c.VRAMKnown {
			return 0, false, "available GPU memory is unknown"
		}
		return c.VRAMAvailable, true, "available memory on one GPU for full offload"
	}
	return c.RAMAvailable, c.RAMKnown, "available system RAM"
}

func modelMemoryRequirement(m model) int64 {
	if m.Size <= 0 {
		return 0
	}
	margin := m.Size / 4
	minimum := int64(2 << 30)
	if strings.HasPrefix(m.Name, "gpt-oss-") {
		minimum = 4 << 30
	}
	if margin < minimum {
		margin = minimum
	}
	return m.Size + margin
}

func recommendModel(catalog []model, c memoryCapacity, backend string) (*model, string) {
	budget, known, kind := capacityBudget(c, backend)
	if !known {
		return nil, kind + "; choose a model explicitly"
	}
	var fallback *model
	for _, m := range catalog {
		required := modelMemoryRequirement(m)
		if required == 0 || required > budget || m.Name == "gpt-oss-120b" {
			continue
		}
		if m.Name == "gpt-oss-20b" {
			return &m, "preferred GPT-OSS 20B fits the conservative memory estimate"
		}
		if strings.HasPrefix(m.Name, "qwen") && (fallback == nil || m.Size > fallback.Size) {
			copy := m
			fallback = &copy
		}
	}
	if fallback != nil {
		return fallback, "GPT-OSS 20B does not fit; this is the largest fitting Qwen fallback"
	}
	return nil, "no catalog model fits the current conservative memory estimate"
}

func (i *installer) checkModelCapacity(m model) error {
	required := modelMemoryRequirement(m)
	if required == 0 {
		fmt.Fprintln(i.out, "Model byte size is unknown; memory fit cannot be estimated before download.")
		return nil
	}
	budget, known, kind := capacityBudget(i.capacity, i.backend)
	if !known {
		total := i.capacity.VRAMTotal
		if i.backend == "cpu" || (i.backend == "metal" && i.capacity.Unified) {
			total = i.capacity.RAMTotal
		}
		if total > 0 && total < required {
			budget, known, kind = total, true, "total installed memory (available memory unknown)"
		}
	}
	if !known {
		fmt.Fprintf(i.out, "Memory fit unknown: %s; estimated model requirement %s (weights plus context/workspace margin).\n", kind, humanBytes(required))
		return nil
	}
	if required > budget {
		message := fmt.Sprintf("%s needs approximately %s including context/workspace; %s is %.2f GiB", m.Name, humanBytes(required), kind, float64(budget)/(1<<30))
		if !i.opts.allowLowMemory {
			return errors.New(message + "; choose a smaller model or explicitly use --allow-low-memory (partial offload/tuning may be needed)")
		}
		fmt.Fprintln(i.out, "Low-memory override:", message)
	}
	return nil
}

func reportCapacity(out io.Writer, c memoryCapacity, catalog []model, backend string) {
	ram, vram := "unknown", "unknown"
	if c.RAMKnown {
		ram = fmt.Sprintf("%.2f GiB", float64(c.RAMAvailable)/(1<<30))
	}
	if c.VRAMKnown {
		vram = fmt.Sprintf("%.2f GiB", float64(c.VRAMAvailable)/(1<<30))
	}
	fmt.Fprintf(out, "Memory: available RAM=%s; GPU memory/budget=%s; unified=%t. Estimates include weights plus context/workspace headroom.\n", ram, vram, c.Unified)
	if c.Source != "" {
		fmt.Fprintf(out, "Capacity probe: %s.\n", c.Source)
	}
	if m, reason := recommendModel(catalog, c, backend); m != nil {
		fmt.Fprintf(out, "Recommended model: %s (%s). %s. GPT-OSS 120B is available by explicit selection.\n", m.Name, humanBytes(modelMemoryRequirement(*m)), reason)
	} else {
		fmt.Fprintf(out, "Model recommendation unavailable: %s.\n", reason)
	}
}

type dependencyCommand struct {
	name string
	args []string
}

func dependencyCommands(goos, manager string, compilerMissing bool) ([]dependencyCommand, error) {
	switch goos {
	case "linux":
		switch manager {
		case "apt-get", "apt":
			return []dependencyCommand{{manager, []string{"update"}}, {manager, []string{"install", "-y", "build-essential", "cmake"}}}, nil
		case "dnf", "yum", "microdnf":
			return []dependencyCommand{{manager, []string{"install", "-y", "gcc", "gcc-c++", "cmake", "make"}}}, nil
		}
	case "darwin":
		if manager == "brew" {
			commands := []dependencyCommand{{"brew", []string{"install", "cmake"}}}
			if compilerMissing {
				commands = append(commands, dependencyCommand{"xcode-select", []string{"--install"}})
			}
			return commands, nil
		}
	case "windows":
		switch manager {
		case "winget":
			commands := []dependencyCommand{{"winget", []string{"install", "--exact", "--id", "Kitware.CMake", "--accept-package-agreements", "--accept-source-agreements"}}}
			if compilerMissing {
				commands = append(commands, dependencyCommand{"winget", []string{"install", "--exact", "--id", "Microsoft.VisualStudio.2022.BuildTools", "--accept-package-agreements", "--accept-source-agreements", "--override", "--wait --passive --add Microsoft.VisualStudio.Workload.VCTools --includeRecommended"}})
			}
			return commands, nil
		case "choco":
			args := []string{"install", "-y", "cmake"}
			if compilerMissing {
				args = append(args, "visualstudio2022buildtools", "visualstudio2022-workload-vctools")
			}
			return []dependencyCommand{{"choco", args}}, nil
		case "scoop":
			return []dependencyCommand{{"scoop", []string{"install", "cmake"}}}, nil
		}
	}
	return nil, fmt.Errorf("no supported automatic prerequisite action for this host. %s", toolGuidance(goos, manager))
}

func (i *installer) installDependencies(h hardware) error {
	if !i.opts.installDeps {
		return errors.New("dependency installation requires explicit --install-deps")
	}
	if i.opts.nonInteractive && h.OS == "darwin" && !h.CXX {
		return errors.New("Apple's compiler installation requires its system dialog; run xcode-select --install first, then rerun this non-interactive installation")
	}
	commands, err := dependencyCommands(h.OS, h.PackageManager, !h.CXX)
	if err != nil {
		return err
	}
	for _, command := range commands {
		name, args := command.name, command.args
		if name == "winget" && i.opts.nonInteractive {
			args = append(args, "--disable-interactivity", "--silent")
			for n := range args {
				args[n] = strings.ReplaceAll(args[n], "--wait --passive", "--wait --quiet")
			}
		}
		if h.OS == "linux" && os.Geteuid() != 0 {
			if _, err := i.lookup("sudo"); err != nil {
				return fmt.Errorf("--install-deps requires administrator privileges; sudo is unavailable. %s", toolGuidance(h.OS, h.PackageManager))
			}
			// The opt-in permits package changes, but never an unattended password
			// prompt. The user can authenticate sudo first or run as administrator.
			args = append([]string{"-n", "--", name}, args...)
			name = "sudo"
		}
		fmt.Fprintf(i.out, "Requested dependency installation: %s %s\n", name, strings.Join(args, " "))
		if h.OS == "windows" {
			fmt.Fprintln(i.out, "Package installation may require administrator approval. Open a fresh Developer PowerShell after installing the C++ workload.")
		}
		if err := i.command("", nil, name, args...); err != nil {
			return fmt.Errorf("dependency installation did not complete; administrator authentication or a new compiler shell may be needed: %w", err)
		}
	}
	return nil
}
