package llama

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// childEnvironment limits launch configuration to the Go owner's explicit
// arguments and exposes colocated native libraries to this child only.
func childEnvironment(executable string, inherited []string, platform string) ([]string, error) {
	if !filepath.IsAbs(executable) {
		return nil, fmt.Errorf("llama.cpp executable must be absolute before preparing child environment")
	}
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("llama.cpp executable must name an existing regular file before preparing child environment")
	}
	key, separator := "", ""
	switch platform {
	case "linux":
		key, separator = "LD_LIBRARY_PATH", ":"
	case "windows":
		key, separator = "PATH", ";"
	}
	directory := filepath.Dir(executable)
	if separator != "" && strings.Contains(directory, separator) {
		return nil, fmt.Errorf("llama.cpp executable directory contains the %s list separator %q", key, separator)
	}
	child := make([]string, 0, len(inherited)+1)
	var existing string
	for _, entry := range inherited {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		// New llama.cpp releases expose models, network settings, and built-in
		// tools through these variables. They must not override our text-only
		// runtime's model, listener, parser, and inference settings.
		if strings.HasPrefix(strings.ToUpper(name), "LLAMA_ARG_") {
			continue
		}
		matches := name == key && key != ""
		if platform == "windows" && key != "" {
			matches = strings.EqualFold(name, key)
		}
		if matches {
			existing = value
			continue
		}
		child = append(child, entry)
	}
	if key != "" {
		value := directory
		if existing != "" {
			value += separator + existing
		}
		child = append(child, key+"="+value)
	}
	// On Darwin, use the install names and rpaths shipped with the native
	// package; do not introduce a process-wide DYLD_LIBRARY_PATH override.
	return child, nil
}
