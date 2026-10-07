package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBackendSelection(t *testing.T) {
	cases := []struct {
		name, request, want string
		hw                  hardware
		fail                bool
	}{
		{"Apple silicon", "auto", "metal", hardware{OS: "darwin", Arch: "arm64", Metal: true}, false},
		{"NVIDIA with toolkit", "auto", "cuda", hardware{NVIDIA: true, CUDA: true}, false},
		{"NVIDIA without toolkit", "auto", "cpu", hardware{NVIDIA: true}, false},
		{"No GPU", "auto", "cpu", hardware{}, false},
		{"Explicit CUDA missing toolkit", "cuda", "", hardware{NVIDIA: true}, true},
		{"Explicit Metal on Linux", "metal", "", hardware{OS: "linux"}, true},
		{"Explicit Vulkan missing SDK", "vulkan", "", hardware{}, true},
		{"Explicit Vulkan tools", "vulkan", "vulkan", hardware{Vulkan: true}, false},
		{"Unknown", "magic", "", hardware{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, e := chooseBackend(c.request, c.hw)
			if (e != nil) != c.fail || p.Name != c.want {
				t.Fatalf("plan=%+v err=%v", p, e)
			}
			if c.name == "NVIDIA without toolkit" && !strings.Contains(p.Reason, "missing") {
				t.Fatal("fallback must explain missing toolkit")
			}
		})
	}
}

func TestPackageManagerDetection(t *testing.T) {
	cases := []struct {
		name, goos, release, available, want string
	}{
		{"Ubuntu prefers apt-get", "linux", "ID=ubuntu\nID_LIKE=debian", "apt-get apt dnf", "apt-get"},
		{"Ubuntu apt fallback", "linux", "ID=ubuntu", "apt dnf", "apt"},
		{"RHEL ignores incidental apt", "linux", "ID=rhel", "apt-get dnf yum", "dnf"},
		{"RHEL yum fallback", "linux", "ID=rhel", "apt-get yum", "yum"},
		{"RHEL minimal", "linux", "ID=rhel", "apt-get microdnf", "microdnf"},
		{"ID precedes ID_LIKE", "linux", "ID=ubuntu\nID_LIKE=rhel", "dnf apt-get", "apt-get"},
		{"Quoted derivative", "linux", "ID=custom\nID_LIKE=\"rhel fedora\"", "apt-get dnf", "dnf"},
		{"Single quoted derivative", "linux", "ID='custom'\nID_LIKE='debian rhel'", "apt-get dnf", "apt-get"},
		{"Missing distro uses PATH", "linux", "", "yum", "yum"},
		{"Unknown distro uses PATH", "linux", "ID=custom", "apt", "apt"},
		{"Malformed distro uses PATH", "linux", "ID=\"unterminated", "dnf", "dnf"},
		{"Known distro lacks native manager", "linux", "ID=rhel", "apt-get", ""},
		{"No manager", "linux", "ID=debian", "", ""},
		{"macOS Homebrew", "darwin", "", "brew apt-get", "brew"},
		{"macOS without Homebrew", "darwin", "", "apt-get", ""},
		{"Windows prefers winget", "windows", "", "winget choco scoop", "winget"},
		{"Windows Chocolatey fallback", "windows", "", "choco scoop", "choco"},
		{"Windows Scoop fallback", "windows", "", "scoop", "scoop"},
		{"Windows no supported manager", "windows", "", "apt-get brew", ""},
		{"Other platform", "freebsd", "", "apt-get brew", ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			available := make(map[string]bool)
			for _, command := range strings.Fields(test.available) {
				available[command] = true
			}
			lookup := func(command string) (string, error) {
				if available[command] {
					return command, nil
				}
				return "", os.ErrNotExist
			}
			if got := detectPackageManager(test.goos, test.release, lookup); got != test.want {
				t.Fatalf("detected %q; want %q", got, test.want)
			}
		})
	}
}

func TestPackageManagerPrerequisiteGuidance(t *testing.T) {
	for _, manager := range []string{"apt-get", "apt", "dnf", "yum", "microdnf"} {
		guidance := toolGuidance("linux", manager)
		if !strings.Contains(guidance, manager+" install") || !strings.Contains(guidance, "cmake") {
			t.Fatalf("missing actionable %s instructions: %s", manager, guidance)
		}
	}
	for _, manager := range []string{"winget", "choco", "scoop"} {
		guidance := toolGuidance("windows", manager)
		if !strings.Contains(guidance, manager+" install") || !strings.Contains(guidance, "Desktop development with C++") || !strings.Contains(guidance, "Developer PowerShell") {
			t.Fatalf("missing Windows prerequisites for %s: %s", manager, guidance)
		}
	}
	if guidance := toolGuidance("darwin", ""); strings.Contains(guidance, "brew") || !strings.Contains(guidance, "https://cmake.org/download/") || !strings.Contains(guidance, "xcode-select --install") {
		t.Fatalf("macOS without Homebrew lacks usable manual instructions: %s", guidance)
	}
	if guidance := toolGuidance("darwin", "brew"); !strings.Contains(guidance, "brew install cmake") {
		t.Fatalf("Homebrew instructions missing: %s", guidance)
	}
	if guidance := toolGuidance("linux", ""); strings.Contains(guidance, "apt") || strings.Contains(guidance, "dnf") || !strings.Contains(guidance, "supported installation instructions") {
		t.Fatalf("unknown Linux manager did not use manual guidance: %s", guidance)
	}
}

func TestDownloadVerifiesAndPreservesPriorFile(t *testing.T) {
	data := []byte("verified artifact")
	requests := 0
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.Write(data) }))
	defer s.Close()
	client := s.Client()
	client.CheckRedirect = secureRedirect
	i := installer{client: client, out: io.Discard}
	dest := filepath.Join(t.TempDir(), "artifact")
	if e := os.WriteFile(dest, []byte("old artifact"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := i.download(s.URL, dest, strings.Repeat("0", 64), 0); e == nil {
		t.Fatal("bad checksum accepted")
	}
	b, _ := os.ReadFile(dest)
	if string(b) != "old artifact" {
		t.Fatal("failed verification changed previous file")
	}
	if e := i.download(s.URL, dest, hashBytes(data), int64(len(data))); e != nil {
		t.Fatal(e)
	}
	before := requests
	if e := i.download(s.URL, dest, hashBytes(data), int64(len(data))); e != nil {
		t.Fatal(e)
	}
	if requests != before {
		t.Fatal("valid cached artifact downloaded again")
	}
	if e := i.download(s.URL, dest, hashBytes(data), int64(len(data)+1)); e == nil {
		t.Fatal("bad expected size accepted")
	}
	b, _ = os.ReadFile(dest)
	if !bytes.Equal(b, data) {
		t.Fatal("failed size verification damaged cache")
	}
}

func TestDownloadRejectsInsecureRedirect(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("artifact")) }))
	defer httpServer.Close()
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, httpServer.URL, http.StatusFound) }))
	defer tlsServer.Close()
	client := tlsServer.Client()
	client.CheckRedirect = secureRedirect
	i := installer{client: client, out: io.Discard}
	if e := i.download(tlsServer.URL, filepath.Join(t.TempDir(), "artifact"), hashBytes([]byte("artifact")), 0); e == nil {
		t.Fatal("insecure redirect accepted")
	}
}

func TestURLSafety(t *testing.T) {
	for _, u := range []string{"http://example.com/a", "file:///tmp/model", "https://user:password@example.com/a", "https://example.com/a#fragment", "https:///missing-host"} {
		if e := validateURL(u); e == nil {
			t.Errorf("unsafe URL accepted: %s", u)
		}
	}
	if e := validateURL("https://example.com/a?download=true"); e != nil {
		t.Fatal(e)
	}
}

func TestArchivePathSafety(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"../outside", "root/../../outside", "/absolute", "C:/absolute", "root\\outside", "root/../a/../../outside"} {
		if _, e := safeArchivePath(root, name, true); e == nil {
			t.Errorf("unsafe archive name accepted %q", name)
		}
	}
	p, e := safeArchivePath(root, "runtime/cmd/fabrics/main.go", true)
	if e != nil || p != filepath.Join(root, "cmd", "fabrics", "main.go") {
		t.Fatalf("%q %v", p, e)
	}
}

func tarArchive(t *testing.T, name string, typ byte) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	h := &tar.Header{Name: name, Typeflag: typ, Mode: 0700}
	if typ == tar.TypeReg {
		h.Size = 3
	}
	if typ == tar.TypeSymlink {
		h.Linkname = "../../outside"
	}
	if e := tw.WriteHeader(h); e != nil {
		t.Fatal(e)
	}
	if typ == tar.TypeReg {
		tw.Write([]byte("abc"))
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}

func TestTarExtractionRejectsLinksAndTraversal(t *testing.T) {
	for _, c := range []struct {
		name string
		typ  byte
	}{{"runtime/link", tar.TypeSymlink}, {"runtime/../../outside", tar.TypeReg}, {"runtime/link", tar.TypeLink}} {
		if e := extractTarGzip(bytes.NewReader(tarArchive(t, c.name, c.typ)), t.TempDir(), true); e == nil {
			t.Fatalf("unsafe archive entry accepted %+v", c)
		}
	}
	d := t.TempDir()
	if e := extractTarGzip(bytes.NewReader(tarArchive(t, "runtime/cmd/main.go", tar.TypeReg)), d, true); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(filepath.Join(d, "cmd", "main.go"))
	if string(b) != "abc" {
		t.Fatal("incorrect file contents")
	}
}

func TestZipExtractionRejectsSymlinks(t *testing.T) {
	file := filepath.Join(t.TempDir(), "archive.zip")
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	h := &zip.FileHeader{Name: "go/link"}
	h.SetMode(os.ModeSymlink | 0700)
	f, e := w.CreateHeader(h)
	if e != nil {
		t.Fatal(e)
	}
	f.Write([]byte("../../outside"))
	w.Close()
	os.WriteFile(file, b.Bytes(), 0600)
	if e := extractZip(file, t.TempDir(), true); e == nil {
		t.Fatal("ZIP symlink accepted")
	}
}

func TestModelSelection(t *testing.T) {
	file := filepath.Join(t.TempDir(), "small.gguf")
	os.WriteFile(file, ggufFixture(), 0600)
	i := installer{opts: options{model: file, nonInteractive: true}, out: io.Discard}
	m, local, e := i.selectModel(nil)
	if e != nil || m != nil || local != file {
		t.Fatalf("%+v %s %v", m, local, e)
	}
	i.opts = options{nonInteractive: true}
	if _, _, e := i.selectModel(nil); e == nil {
		t.Fatal("noninteractive installation prompted")
	}
	i.opts = options{modelURL: "https://example.com/model.gguf"}
	if _, _, e := i.selectModel(nil); e == nil {
		t.Fatal("unverified model download accepted")
	}
	i.opts = options{skipModel: true, model: file}
	if _, _, e := i.selectModel(nil); e == nil {
		t.Fatal("contradictory options accepted")
	}
	i.opts = options{}
	i.in = strings.NewReader("1\n" + file + "\n")
	_, local, e = i.selectModel(nil)
	if e != nil || local != file {
		t.Fatalf("interactive local choice: %s %v", local, e)
	}
}

func ggufFixture() []byte {
	return append([]byte("GGUF\x03\x00\x00\x00"), make([]byte, 16)...)
}

func TestGGUFRejectsTruncatedHeader(t *testing.T) {
	file := filepath.Join(t.TempDir(), "model.gguf")
	for _, size := range []int{0, 8, 23} {
		if err := os.WriteFile(file, ggufFixture()[:size], 0600); err != nil {
			t.Fatal(err)
		}
		if err := checkGGUF(file); err == nil {
			t.Fatalf("accepted truncated %d-byte model header", size)
		}
	}
}

func TestModelRevisionsPreservePreviousBytes(t *testing.T) {
	prefix := t.TempDir()
	legacy := filepath.Join(prefix, "models", "test.gguf")
	if err := os.MkdirAll(filepath.Dir(legacy), 0700); err != nil {
		t.Fatal(err)
	}
	old := append(ggufFixture(), []byte("old model")...)
	if err := os.WriteFile(legacy, old, 0600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	latest := append(ggufFixture(), []byte("new model")...)
	client := &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(latest)), Header: make(http.Header), Request: r}, nil
	})}
	i := installer{opts: options{prefix: prefix}, client: client, out: io.Discard}
	selection := model{Name: "test", URL: "https://example.com/model.gguf", SHA256: hashBytes(old), Size: int64(len(old))}
	oldPath, err := i.downloadModel(selection)
	if err != nil || oldPath == legacy || requests != 0 {
		t.Fatalf("verified legacy model not reused without download: %s %v (%d requests)", oldPath, err, requests)
	}
	selection.SHA256, selection.Size = hashBytes(latest), int64(len(latest))
	newPath, err := i.downloadModel(selection)
	if err != nil || newPath == oldPath || requests != 1 {
		t.Fatalf("new revision not downloaded to a separate path: %s %v (%d requests)", newPath, err, requests)
	}
	for _, file := range []string{legacy, oldPath} {
		data, err := os.ReadFile(file)
		if err != nil || !bytes.Equal(data, old) {
			t.Fatalf("previous model changed: %s %v", file, err)
		}
	}
	data, err := os.ReadFile(newPath)
	if err != nil || !bytes.Equal(data, latest) {
		t.Fatalf("new model content incorrect: %v", err)
	}
}

func TestSameBackendReinstallPreservesGPULayerTuning(t *testing.T) {
	config := map[string]any{"backend": "cuda", "gpu_layers": float64(12), "model_path": "existing.gguf"}
	applyBackendConfig(config, "cuda")
	if config["gpu_layers"] != float64(12) || config["model_path"] != "existing.gguf" {
		t.Fatalf("reinstall lost configuration: %+v", config)
	}
	applyBackendConfig(config, "cpu")
	if config["gpu_layers"] != 0 || config["backend"] != "cpu" {
		t.Fatalf("CPU switch kept GPU offload: %+v", config)
	}
	applyBackendConfig(config, "vulkan")
	if config["gpu_layers"] != -1 || config["backend"] != "vulkan" {
		t.Fatalf("GPU switch lacks default offload: %+v", config)
	}
}

func TestPublishFilesRollsBackWholeInstallation(t *testing.T) {
	for _, failure := range []string{"missing source", "blocked destination"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			var replacements []fileReplacement
			for _, name := range []string{"fabrics", "llama-server", "config.json", "llama-build.json"} {
				source, dest := filepath.Join(dir, "new-"+name), filepath.Join(dir, name)
				if err := os.WriteFile(source, []byte("new "+name), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dest, []byte("old "+name), 0600); err != nil {
					t.Fatal(err)
				}
				replacements = append(replacements, fileReplacement{source, dest})
			}
			last := replacements[len(replacements)-1]
			if failure == "missing source" {
				os.Remove(last.source)
			} else {
				os.Remove(last.destination)
				os.Mkdir(last.destination, 0700)
			}
			if err := publishFiles(replacements, io.Discard); err == nil {
				t.Fatal("failed update reported success")
			}
			for _, file := range replacements[:len(replacements)-1] {
				data, err := os.ReadFile(file.destination)
				if err != nil || string(data) != "old "+filepath.Base(file.destination) {
					t.Fatalf("failed update changed %s: %s %v", file.destination, data, err)
				}
			}
			backups, err := filepath.Glob(filepath.Join(dir, ".previous-*"))
			if err != nil || len(backups) != 0 {
				t.Fatalf("successful rollback left backups: %v %v", backups, err)
			}
		})
	}
}

func TestPublishFilesCommitsAndRollsBackNewFiles(t *testing.T) {
	dir := t.TempDir()
	one := fileReplacement{filepath.Join(dir, "new-one"), filepath.Join(dir, "one")}
	two := fileReplacement{filepath.Join(dir, "new-two"), filepath.Join(dir, "two")}
	os.WriteFile(one.source, []byte("one"), 0600)
	if err := publishFiles([]fileReplacement{one, two}, io.Discard); err == nil {
		t.Fatal("missing second source accepted")
	}
	if _, err := os.Stat(one.destination); !os.IsNotExist(err) {
		t.Fatal("rollback left newly installed file")
	}
	os.WriteFile(one.source, []byte("one"), 0600)
	os.WriteFile(two.source, []byte("two"), 0600)
	if err := publishFiles([]fileReplacement{one, two}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, item := range []fileReplacement{one, two} {
		data, err := os.ReadFile(item.destination)
		if err != nil || string(data) != filepath.Base(item.destination) {
			t.Fatalf("file was not installed: %s %v", item.destination, err)
		}
	}
}

func TestCatalogRequiresImmutableRevision(t *testing.T) {
	models := []model{{Name: "tiny", URL: "https://huggingface.co/org/model/resolve/main/model.gguf", SHA256: strings.Repeat("a", 64)}}
	encode := func() string { b, _ := json.Marshal(models); return base64.StdEncoding.EncodeToString(b) }
	if _, e := loadCatalog(encode()); e == nil {
		t.Fatal("mutable catalog revision accepted")
	}
	models[0].URL = "https://huggingface.co/org/model/resolve/" + strings.Repeat("b", 40) + "/model.gguf"
	if _, e := loadCatalog(encode()); e != nil {
		t.Fatal(e)
	}
	models = append(models, models[0])
	if _, e := loadCatalog(encode()); e == nil {
		t.Fatal("duplicate catalog name accepted")
	}
}

func TestDoctorDoesNotWrite(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "must-not-exist")
	var out bytes.Buffer
	if e := run([]string{"--doctor", "--backend", "cpu", "--prefix", prefix}, strings.NewReader(""), &out); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(prefix); !os.IsNotExist(e) {
		t.Fatal("doctor wrote files")
	}
	if !strings.Contains(out.String(), llamaCommit) {
		t.Fatal("doctor omitted source pin")
	}
	if !strings.Contains(out.String(), "Package manager: ") {
		t.Fatal("doctor omitted package-manager status")
	}
}

func TestDoctorReportsMissingPackageManagerWithoutWriting(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	prefix := filepath.Join(t.TempDir(), "must-not-exist")
	var out bytes.Buffer
	if err := run([]string{"--doctor", "--backend", "cpu", "--prefix", prefix}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Package manager: none detected") {
		t.Fatalf("doctor omitted missing-manager status: %s", out.String())
	}
	if _, err := os.Stat(prefix); !os.IsNotExist(err) {
		t.Fatal("doctor created installation files")
	}
}

func TestGoVersionGate(t *testing.T) {
	for _, s := range []string{"go version go1.24.0 linux/amd64", "go version go1.27.1 windows/arm64"} {
		if !sufficientGoVersion(s) {
			t.Fatalf("suitable Go rejected %s", s)
		}
	}
	for _, s := range []string{"go version go1.23.9 linux/amd64", "garbage", "go version devel go1.24 linux/amd64"} {
		if s == "go version devel go1.24 linux/amd64" {
			continue
		}
		if sufficientGoVersion(s) {
			t.Fatalf("unsuitable Go accepted %s", s)
		}
	}
}

func TestReplaceFileAndPreserveConfigFields(t *testing.T) {
	d := t.TempDir()
	dest := filepath.Join(d, "file")
	src := filepath.Join(d, "replacement")
	os.WriteFile(dest, []byte("before"), 0600)
	os.WriteFile(src, []byte("after"), 0600)
	if e := replaceFile(src, dest); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(dest)
	if string(b) != "after" {
		t.Fatal("replacement failed")
	}
	config := filepath.Join(d, "config.json")
	os.WriteFile(config, []byte(`{"model_path":"mine.gguf","custom_setting":true}`), 0600)
	c, e := readConfig(config)
	if e != nil || c["custom_setting"] != true {
		t.Fatalf("%+v %v", c, e)
	}
	os.WriteFile(config, []byte("not json"), 0600)
	if _, e = readConfig(config); e == nil {
		t.Fatal("invalid configuration accepted")
	}
}

func TestLlamaReuseRequiresMatchingProfileAndIntegrity(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0700)
	data := []byte("previously verified build")
	file := filepath.Join(bin, exeName("llama-server"))
	os.WriteFile(file, data, 0700)
	cpu := currentCPUIdentity()
	if cpu == "" {
		t.Skip("platform does not expose CPU identity; verified build reuse is intentionally disabled")
	}
	manifest := llamaManifest{SourceSHA256: llamaSHA256, BuildProfile: llamaBuildProfile, CPUIdentity: cpu, Backend: "cpu", Platform: runtime.GOOS + "/" + runtime.GOARCH, Files: map[string]string{exeName("llama-server"): hashBytes(data)}}
	encoded, _ := json.Marshal(manifest)
	os.WriteFile(filepath.Join(dir, "llama-build.json"), encoded, 0600)
	if _, ok := reusableLlama(dir, "cpu"); !ok {
		t.Fatal("valid prior build not reused")
	}
	if _, ok := reusableLlama(dir, "cuda"); ok {
		t.Fatal("different backend reused")
	}
	manifest.CPUIdentity = "different-machine"
	encoded, _ = json.Marshal(manifest)
	os.WriteFile(filepath.Join(dir, "llama-build.json"), encoded, 0600)
	if _, ok := reusableLlama(dir, "cpu"); ok {
		t.Fatal("native build reused on a different CPU")
	}
	manifest.CPUIdentity = cpu
	encoded, _ = json.Marshal(manifest)
	os.WriteFile(filepath.Join(dir, "llama-build.json"), encoded, 0600)
	os.WriteFile(file, []byte("modified"), 0700)
	if _, ok := reusableLlama(dir, "cpu"); ok {
		t.Fatal("modified binary reused")
	}
	os.WriteFile(file, data, 0700)
	manifest.Files["../outside"] = hashBytes(data)
	encoded, _ = json.Marshal(manifest)
	os.WriteFile(filepath.Join(dir, "llama-build.json"), encoded, 0600)
	if _, ok := reusableLlama(dir, "cpu"); ok {
		t.Fatal("unsafe manifest file path accepted")
	}
}

func TestCUDAReuseRequiresSameDeviceAndDriver(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture; CUDA fingerprint checks are platform-independent")
	}
	cpu := currentCPUIdentity()
	if cpu == "" {
		t.Skip("CPU identity unavailable")
	}
	dir := t.TempDir()
	probe := filepath.Join(dir, "nvidia-smi")
	writeProbe := func(output string) {
		t.Helper()
		if err := os.WriteFile(probe, []byte("#!/bin/sh\nprintf '%s\\n' '"+output+"'\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeProbe("GPU one, 8.6, 580.0")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0700)
	data := []byte("verified CUDA build")
	os.WriteFile(filepath.Join(bin, exeName("llama-server")), data, 0700)
	manifest := llamaManifest{SourceSHA256: llamaSHA256, BuildProfile: llamaBuildProfile, CPUIdentity: cpu, CUDAIdentity: currentCUDAIdentity("cuda"), Backend: "cuda", Platform: runtime.GOOS + "/" + runtime.GOARCH, Files: map[string]string{exeName("llama-server"): hashBytes(data)}}
	encoded, _ := json.Marshal(manifest)
	os.WriteFile(filepath.Join(dir, "llama-build.json"), encoded, 0600)
	if _, ok := reusableLlama(dir, "cuda"); !ok {
		t.Fatal("unchanged CUDA build not reused")
	}
	for _, output := range []string{"GPU two, 9.0, 580.0", "GPU one, 8.6, 590.0", ""} {
		writeProbe(output)
		if _, ok := reusableLlama(dir, "cuda"); ok {
			t.Fatalf("CUDA build reused after changed/unavailable device identity %q", output)
		}
	}
	writeProbe("GPU one, 8.6, 580.0")
	t.Setenv("CUDA_VISIBLE_DEVICES", "different-visible-device")
	if _, ok := reusableLlama(dir, "cuda"); ok {
		t.Fatal("CUDA build reused after changing visible GPU selection")
	}
}

type fixtureTransport func(*http.Request) (*http.Response, error)

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func metadataFixture(candidate model) map[string]any {
	return map[string]any{"id": candidate.Repository, "sha": strings.Repeat("b", 40), "siblings": []any{map[string]any{"rfilename": candidate.Filename, "size": 12, "lfs": map[string]any{"sha256": strings.Repeat("a", 64), "size": 12}}}}
}

func TestOfficialMetadataValidation(t *testing.T) {
	candidate := modelCandidates()[0]
	data, _ := json.Marshal(metadataFixture(candidate))
	pinned, err := parseModelMetadata(candidate, data)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Revision != strings.Repeat("b", 40) || pinned.SHA256 != strings.Repeat("a", 64) || pinned.Size != 12 || pinned.URL != "https://huggingface.co/"+candidate.Repository+"/resolve/"+strings.Repeat("b", 40)+"/"+candidate.Filename {
		t.Fatalf("incorrect immutable artifact %+v", pinned)
	}
	cases := []struct {
		name string
		edit func(map[string]any)
	}{
		{"wrong repository", func(m map[string]any) { m["id"] = "another/model" }},
		{"mutable revision", func(m map[string]any) { m["sha"] = "main" }},
		{"missing filename", func(m map[string]any) { m["siblings"] = []any{} }},
		{"missing LFS", func(m map[string]any) { m["siblings"].([]any)[0].(map[string]any)["lfs"] = nil }},
		{"missing SHA", func(m map[string]any) {
			m["siblings"].([]any)[0].(map[string]any)["lfs"].(map[string]any)["sha256"] = ""
		}},
		{"missing size", func(m map[string]any) { m["siblings"].([]any)[0].(map[string]any)["lfs"].(map[string]any)["size"] = 0 }},
		{"inconsistent size", func(m map[string]any) { m["siblings"].([]any)[0].(map[string]any)["size"] = 100 }},
		{"duplicate file", func(m map[string]any) { a := m["siblings"].([]any); m["siblings"] = append(a, a[0]) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := metadataFixture(candidate)
			c.edit(m)
			b, _ := json.Marshal(m)
			if _, err := parseModelMetadata(candidate, b); err == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
}

func TestModelResolutionPinsOnceAndPreservesInvalidManifest(t *testing.T) {
	candidate := modelCandidates()[0]
	calls := 0
	client := &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://huggingface.co/api/models/"+candidate.Repository+"?blobs=true" {
			t.Fatalf("unexpected metadata endpoint %s", r.URL)
		}
		data, _ := json.Marshal(metadataFixture(candidate))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data)), Header: make(http.Header), Request: r}, nil
	})}
	i := installer{opts: options{prefix: t.TempDir()}, client: client, out: io.Discard}
	pinned, err := i.resolveSelectedModel(candidate)
	if err != nil {
		t.Fatal(err)
	}
	second, err := i.resolveSelectedModel(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || *pinned != *second {
		t.Fatal("rerun changed the pinned model or fetched mutable discovery again")
	}
	manifest := filepath.Join(i.opts.prefix, "models", candidate.Name+".manifest.json")
	os.WriteFile(manifest, []byte(`{"revision":"main"}`), 0600)
	if _, err := i.resolveSelectedModel(candidate); err == nil {
		t.Fatal("invalid cached model pin accepted")
	}
	b, _ := os.ReadFile(manifest)
	if string(b) != `{"revision":"main"}` || calls != 1 {
		t.Fatal("invalid model pin was silently overwritten or refetched")
	}
}

func TestModelMetadataNetworkFailureIsExplicit(t *testing.T) {
	candidate := modelCandidates()[0]
	client := &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("denied")), Request: r}, nil
	})}
	i := installer{opts: options{prefix: t.TempDir()}, client: client, out: io.Discard}
	if _, err := i.resolveSelectedModel(candidate); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("expected clear metadata network failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(i.opts.prefix, "models")); !os.IsNotExist(err) {
		t.Fatal("failed resolution saved model metadata")
	}
}

func TestBuiltInChoicesAppearOffline(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"--list-models"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	for _, m := range modelCandidates() {
		if !strings.Contains(out.String(), m.Name) || !strings.Contains(out.String(), m.Repository) {
			t.Fatalf("candidate missing from offline list: %s", m.Name)
		}
	}
}

func TestLargeModelSizeUsesDownloadLimit(t *testing.T) {
	candidate := modelCandidates()[2]
	metadata := metadataFixture(candidate)
	file := metadata["siblings"].([]any)[0].(map[string]any)
	file["size"] = int64(3 << 30)
	file["lfs"].(map[string]any)["size"] = int64(3 << 30)
	data, _ := json.Marshal(metadata)
	if _, err := parseModelMetadata(candidate, data); err != nil {
		t.Fatalf("valid model larger than archive limit rejected: %v", err)
	}
	file["size"] = maxDownloadBytes + 1
	file["lfs"].(map[string]any)["size"] = maxDownloadBytes + 1
	data, _ = json.Marshal(metadata)
	if _, err := parseModelMetadata(candidate, data); err == nil {
		t.Fatal("oversized model metadata accepted")
	}
}

func TestGGUFRejectsFIFOWithoutOpeningIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX FIFO test")
	}
	if _, err := exec.LookPath("mkfifo"); err != nil {
		t.Skip("mkfifo unavailable")
	}
	fifo := filepath.Join(t.TempDir(), "model.gguf")
	if err := exec.Command("mkfifo", fifo).Run(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- checkGGUF(fifo) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO accepted as model")
		}
	case <-time.After(time.Second):
		t.Fatal("model validation blocked opening a FIFO")
	}
}

func TestDirectCatalogSelectionPersistsProvenanceWithoutNetwork(t *testing.T) {
	candidate := modelCandidates()[0]
	data, _ := json.Marshal(metadataFixture(candidate))
	pinned, err := parseModelMetadata(candidate, data)
	if err != nil {
		t.Fatal(err)
	}
	i := installer{opts: options{prefix: t.TempDir()}, out: io.Discard}
	selected, err := i.resolveSelectedModel(pinned)
	if err != nil {
		t.Fatal(err)
	}
	if *selected != pinned {
		t.Fatal("direct catalog selection changed the fixed artifact")
	}
	manifest := filepath.Join(i.opts.prefix, "models", pinned.Name+".manifest.json")
	saved, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var stored model
	if err := json.Unmarshal(saved, &stored); err != nil || stored != pinned {
		t.Fatalf("catalog lineage not persisted: %+v %v", stored, err)
	}
}

func nativeFixture(t *testing.T) (nativeManifest, []byte) {
	t.Helper()
	files := map[string][]byte{
		"bin/fabrics":               []byte("#!/bin/sh\nexit 0\n"),
		"backends/cpu/llama-server": []byte("#!/bin/sh\nexit 0\n"),
		"licenses/LICENSE":          []byte("test license"),
	}
	manifest := nativeManifest{FormatVersion: 1, Version: "0.1.0", Platform: runtime.GOOS + "/" + runtime.GOARCH, LlamaCommit: llamaCommit, Backends: []string{"cpu"}, DefaultBackend: "cpu", BackendPaths: map[string]string{"cpu": "backends/cpu/llama-server"}, Files: map[string]string{}}
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	tarWriter := tar.NewWriter(gz)
	for name, content := range files {
		manifest.Files[name] = hashBytes(content)
		if err := tarWriter.WriteHeader(&tar.Header{Name: "native/" + name, Mode: 0700, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return manifest, data.Bytes()
}

func TestNativeManifestAndPayloadValidation(t *testing.T) {
	manifest, archive := nativeFixture(t)
	if runtime.GOOS == "windows" {
		manifest.Platform = "linux/amd64"
	}
	encode := func(m nativeManifest) string {
		data, _ := json.Marshal(m)
		return base64.StdEncoding.EncodeToString(data)
	}
	goos, arch, _ := strings.Cut(manifest.Platform, "/")
	if _, err := loadNativeManifest(encode(manifest), goos, arch); err != nil {
		t.Fatal(err)
	}
	if _, err := loadNativeManifest(encode(manifest), "unsupported", arch); err == nil {
		t.Fatal("wrong platform accepted")
	}
	if err := extractNativePayload(base64.StdEncoding.EncodeToString(archive), hashBytes(archive), t.TempDir(), &manifest); err != nil {
		t.Fatal(err)
	}
	if err := extractNativePayload(base64.StdEncoding.EncodeToString(archive), strings.Repeat("0", 64), t.TempDir(), &manifest); err == nil {
		t.Fatal("unverified native archive accepted")
	}
	manifest.Files["bin/fabrics"] = strings.Repeat("0", 64)
	if err := extractNativePayload(base64.StdEncoding.EncodeToString(archive), hashBytes(archive), t.TempDir(), &manifest); err == nil {
		t.Fatal("native file checksum mismatch accepted")
	}
	manifest.Files["bin/../../outside"] = strings.Repeat("0", 64)
	if _, err := loadNativeManifest(encode(manifest), goos, arch); err == nil {
		t.Fatal("escaping native manifest path accepted")
	}
}

func TestNativeBackendDoesNotRequireBuildToolkits(t *testing.T) {
	manifest := &nativeManifest{BackendPaths: map[string]string{"cpu": "backends/cpu/llama-server", "cuda": "backends/cuda/llama-server", "metal": "backends/metal/llama-server", "vulkan": "backends/vulkan/llama-server"}}
	plan, err := chooseNativeBackend("auto", hardware{OS: "linux", NVIDIA: true, CUDA: false}, manifest)
	if err != nil || plan.Name != "cuda" {
		t.Fatalf("native CUDA incorrectly requires toolkit: %+v %v", plan, err)
	}
	plan, err = chooseNativeBackend("vulkan", hardware{OS: "linux", Vulkan: false}, manifest)
	if err != nil || plan.Name != "vulkan" {
		t.Fatalf("native Vulkan incorrectly requires SDK: %+v %v", plan, err)
	}
	if _, err := chooseNativeBackend("cuda", hardware{OS: "linux"}, manifest); err == nil {
		t.Fatal("CUDA without driver accepted")
	}
	if _, err := chooseNativeBackend("metal", hardware{OS: "linux"}, manifest); err == nil {
		t.Fatal("Metal on Linux accepted")
	}
	plan, err = chooseNativeBackend("auto", hardware{OS: "linux", NVIDIA: true}, &nativeManifest{BackendPaths: map[string]string{"cpu": "backends/cpu/llama-server"}})
	if err != nil || plan.Name != "cpu" || !strings.Contains(plan.Reason, "no CUDA variant") {
		t.Fatalf("CPU-only release did not explain GPU fallback: %+v %v", plan, err)
	}
}

func TestNativeReleaseInstallsWithoutBuildToolsOrNetwork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixtures; manifest tests cover platform rejection")
	}
	manifest, archive := nativeFixture(t)
	oldArchive, oldHash, oldManifest := nativeArchiveBase64, nativeArchiveSHA256, nativeManifestBase64
	t.Cleanup(func() {
		nativeArchiveBase64, nativeArchiveSHA256, nativeManifestBase64 = oldArchive, oldHash, oldManifest
	})
	data, _ := json.Marshal(manifest)
	var provenance map[string]any
	json.Unmarshal(data, &provenance)
	provenance["compiler"] = map[string]any{"name": "fixture compiler"}
	data, _ = json.Marshal(provenance)
	nativeArchiveBase64, nativeArchiveSHA256, nativeManifestBase64 = base64.StdEncoding.EncodeToString(archive), hashBytes(archive), base64.StdEncoding.EncodeToString(data)
	t.Setenv("PATH", t.TempDir())
	prefix := filepath.Join(t.TempDir(), "installation")
	var out bytes.Buffer
	if err := run([]string{"--doctor", "--prefix", prefix, "--backend", "cpu"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(prefix); !os.IsNotExist(err) {
		t.Fatal("native doctor wrote installed files")
	}
	if !strings.Contains(out.String(), "No Go/CMake/C++ installation is required") {
		t.Fatal(out.String())
	}
	if err := run([]string{"--skip-model", "--non-interactive", "--prefix", prefix, "--backend", "cpu"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	config, err := readConfig(filepath.Join(prefix, "config.json"))
	if err != nil || config["llama_path"] != filepath.Join(prefix, "backends", "cpu", "llama-server") || config["cognition_mode"] != "adaptive" {
		t.Fatalf("wrong native configuration: %+v %v", config, err)
	}
	if _, err := os.Stat(filepath.Join(prefix, "licenses", "LICENSE")); err != nil {
		t.Fatal(err)
	}
	release, err := os.ReadFile(filepath.Join(prefix, "native-release.json"))
	if err != nil || !bytes.Contains(release, []byte("fixture compiler")) {
		t.Fatalf("native compiler provenance was not preserved: %s %v", release, err)
	}
	if strings.Contains(out.String(), "Running go") || strings.Contains(out.String(), "Running cmake") {
		t.Fatal("native install invoked build tools")
	}
}

func TestCapacityEstimatesAndRecommendation(t *testing.T) {
	catalog := modelCandidates()
	cases := []struct {
		name          string
		capacity      memoryCapacity
		backend, want string
	}{
		{"preferred GPTOSS", memoryCapacity{RAMAvailable: 20 << 30, RAMKnown: true}, "cpu", "gpt-oss-20b"},
		{"120B stays explicit", memoryCapacity{RAMAvailable: 120 << 30, RAMKnown: true}, "cpu", "gpt-oss-20b"},
		{"small fallback", memoryCapacity{RAMAvailable: 7 << 29, RAMKnown: true}, "cpu", "qwen2.5-1.5b"},
		{"insufficient memory", memoryCapacity{RAMAvailable: 1 << 30, RAMKnown: true}, "cpu", ""},
		{"unknown memory", memoryCapacity{}, "cpu", ""},
		{"GPU full offload capped", memoryCapacity{RAMAvailable: 100 << 30, RAMKnown: true, VRAMAvailable: 7 << 29, VRAMKnown: true}, "cuda", "qwen2.5-1.5b"},
		{"unified memory never summed", memoryCapacity{RAMAvailable: 10 << 30, RAMKnown: true, VRAMAvailable: 10 << 30, VRAMKnown: true, Unified: true}, "metal", "qwen2.5-3b"},
		{"unified GPU budget caps RAM", memoryCapacity{RAMAvailable: 24 << 30, RAMKnown: true, VRAMAvailable: 7 << 29, VRAMKnown: true, Unified: true}, "metal", "qwen2.5-1.5b"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			m, reason := recommendModel(catalog, test.capacity, test.backend)
			got := ""
			if m != nil {
				got = m.Name
			}
			if got != test.want || reason == "" {
				t.Fatalf("got %q (%s), want %q", got, reason, test.want)
			}
		})
	}
	c := parseLinuxMemory("MemTotal: 16777216 kB\nMemAvailable: 10485760 kB\n")
	if c.RAMTotal != 16<<30 || c.RAMAvailable != 10<<30 || !c.RAMKnown {
		t.Fatalf("Linux capacity wrong: %+v", c)
	}
	c = restrictMemory(c, 4<<30, 3<<30)
	if c.RAMTotal != 4<<30 || c.RAMAvailable != 1<<30 {
		t.Fatalf("container memory limit ignored: %+v", c)
	}
	if _, err := readCapacityJSON([]byte(`{"ram_total_bytes":1,"ram_available_bytes":2}`)); err == nil {
		t.Fatal("invalid probe values accepted")
	}
}

func TestExplicitModelCapacityGuardAndUnknown(t *testing.T) {
	i := installer{out: io.Discard, backend: "cpu", capacity: memoryCapacity{RAMKnown: true, RAMAvailable: 4 << 30}}
	gpt := modelCandidates()[0]
	if err := i.checkModelCapacity(gpt); err == nil || !strings.Contains(err.Error(), "--allow-low-memory") {
		t.Fatalf("expected explicit capacity refusal: %v", err)
	}
	i.opts.allowLowMemory = true
	if err := i.checkModelCapacity(gpt); err != nil {
		t.Fatal(err)
	}
	i.opts.allowLowMemory = false
	i.capacity = memoryCapacity{}
	var out bytes.Buffer
	i.out = &out
	if err := i.checkModelCapacity(gpt); err != nil || !strings.Contains(out.String(), "unknown") {
		t.Fatalf("unknown capacity was fabricated: %s %v", out.String(), err)
	}
	i.opts = options{model: "gpt-oss-120b", nonInteractive: true}
	m, _, err := i.selectModel(modelCandidates())
	if err != nil || m.Name != "gpt-oss-120b" {
		t.Fatalf("explicit selection silently changed: %+v %v", m, err)
	}
	i.opts = options{model: "auto", nonInteractive: true}
	i.capacity = memoryCapacity{RAMKnown: true, RAMAvailable: 24 << 30}
	m, _, err = i.selectModel(modelCandidates())
	if err != nil || m.Name != "gpt-oss-20b" {
		t.Fatalf("automatic recommendation failed: %+v %v", m, err)
	}
	i.opts = options{}
	i.in = strings.NewReader("\n")
	m, _, err = i.selectModel(modelCandidates())
	if err != nil || m.Name != "gpt-oss-20b" {
		t.Fatalf("interactive default failed: %+v %v", m, err)
	}
}

func TestDependencyInstallationRequiresOptInAndUsesArgumentVectors(t *testing.T) {
	i := installer{out: io.Discard}
	if err := i.installDependencies(hardware{OS: "linux", PackageManager: "apt-get"}); err == nil {
		t.Fatal("dependencies installed without explicit opt-in")
	}
	commands, err := dependencyCommands("linux", "apt-get", true)
	if err != nil || len(commands) != 2 || commands[1].name != "apt-get" || strings.Join(commands[1].args, " ") != "install -y build-essential cmake" {
		t.Fatalf("unexpected apt plan: %+v %v", commands, err)
	}
	for _, manager := range []string{"dnf", "yum", "microdnf"} {
		commands, err := dependencyCommands("linux", manager, true)
		if err != nil || len(commands) != 1 || commands[0].name != manager {
			t.Fatalf("unexpected %s plan: %+v %v", manager, commands, err)
		}
	}
	if _, err := dependencyCommands("linux", "unknown; shell", true); err == nil {
		t.Fatal("unrecognized manager accepted")
	}
	commands, err = dependencyCommands("windows", "winget", true)
	if err != nil || len(commands) != 2 || !strings.Contains(strings.Join(commands[1].args, " "), "Microsoft.VisualStudio.Workload.VCTools") {
		t.Fatalf("Windows C++ workload missing: %+v %v", commands, err)
	}
}

func TestInsufficientMemoryRefusesBeforeModelDownload(t *testing.T) {
	requests := 0
	i := installer{opts: options{prefix: t.TempDir()}, out: io.Discard, backend: "cpu", capacity: memoryCapacity{RAMAvailable: 1 << 30, RAMKnown: true}, client: &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) { requests++; return nil, os.ErrNotExist })}}
	selected := model{Name: "gpt-oss-20b", URL: "https://example.com/model.gguf", SHA256: strings.Repeat("a", 64), Size: 12109566624}
	if err := i.install(backendPlan{Name: "cpu"}, &selected, ""); err == nil || !strings.Contains(err.Error(), "--allow-low-memory") {
		t.Fatalf("expected capacity refusal: %v", err)
	}
	if requests != 0 {
		t.Fatalf("low-memory selection made %d network requests", requests)
	}
	if _, err := os.Stat(filepath.Join(i.opts.prefix, "config.json")); !os.IsNotExist(err) {
		t.Fatal("refused install changed active configuration")
	}
}
