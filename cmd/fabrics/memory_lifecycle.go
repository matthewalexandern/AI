package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/matthewalexandern/AI/internal/memory"
)

const memoryUsage = `Usage: fabrics [--home DIRECTORY] memory COMMAND [OPTIONS]

  list [--limit 20] [--offset 0]  List stored notes and episodes
  inspect ID                    Read one stored memory
  forget ID                     Delete a note, or its whole conversation turn
  export FILE                   Write a complete portable JSON snapshot
  backup FILE                   Write a consistent SQLite snapshot
  restore [--replace] FILE       Merge a JSON/SQLite snapshot; --replace replaces all memory

Memory commands work without loading a model. Export and backup refuse to overwrite files.
`

func memoryLifecycleCommand(ctx context.Context, home string, args []string, out, errs io.Writer) error {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "--help" || args[0] == "-h")) {
		_, err := fmt.Fprint(out, memoryUsage)
		return err
	}
	command := args[0]
	switch command {
	case "list", "inspect", "forget", "export", "backup", "restore":
	default:
		return fmt.Errorf("unknown memory command %q", command)
	}
	f := flags("memory "+command, errs)
	var limit, offset int
	var replace bool
	if command == "list" {
		f.IntVar(&limit, "limit", 20, "maximum results (1..100)")
		f.IntVar(&offset, "offset", 0, "number of records to skip")
	}
	if command == "restore" {
		f.BoolVar(&replace, "replace", false, "replace all local memory instead of merging")
	}
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if command == "list" {
		if f.NArg() != 0 || limit < 1 || limit > 100 || offset < 0 {
			return errors.New("memory list accepts --limit 1..100 and --offset >=0, without positional arguments")
		}
	} else if f.NArg() != 1 {
		return fmt.Errorf("memory %s requires one %s", command, memoryArgument(command))
	}
	var id int64
	if command == "inspect" || command == "forget" {
		var err error
		id, err = strconv.ParseInt(f.Arg(0), 10, 64)
		if err != nil || id < 1 {
			return errors.New("memory ID must be a positive integer")
		}
	}
	var input *os.File
	if command == "restore" {
		var err error
		input, err = os.Open(f.Arg(0))
		if err != nil {
			return err
		}
		defer input.Close()
		info, err := input.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("restore requires a regular snapshot file")
		}
	}
	store, err := openMemory(home)
	if err != nil {
		return err
	}
	defer store.Close()
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	switch command {
	case "list":
		items, err := store.List(ctx, limit, offset)
		if err != nil {
			return err
		}
		return enc.Encode(items)
	case "inspect":
		item, err := store.Inspect(ctx, id)
		if err != nil {
			return err
		}
		return enc.Encode(item)
	case "forget":
		if err := store.Forget(ctx, id); err != nil {
			return err
		}
		return enc.Encode(map[string]int64{"forgotten": id})
	case "export":
		if err := writeNewSnapshot(f.Arg(0), func(w io.Writer) error { return store.Export(ctx, w) }); err != nil {
			return err
		}
	case "backup":
		if err := store.Backup(ctx, f.Arg(0)); err != nil {
			return err
		}
	case "restore":
		if err := store.Restore(ctx, input, replace); err != nil {
			return err
		}
	}
	return enc.Encode(map[string]any{"status": "completed", "command": command, "path": f.Arg(0), "replace": replace})
}

func memoryArgument(command string) string {
	if command == "inspect" || command == "forget" {
		return "memory ID"
	}
	return "snapshot path"
}

// Publish a complete snapshot without exposing a partial file or overwriting
// an existing backup. The temporary file has private permissions.
func writeNewSnapshot(path string, write func(io.Writer) error) error {
	dest, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("preserving existing snapshot %s", dest)
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(dest), ".memory-export-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if err := write(file); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Link(file.Name(), dest)
}

func registerMemoryLifecycleRoutes(mux *http.ServeMux, store *memory.Store) {
	for _, method := range []string{"GET", "DELETE"} {
		mux.HandleFunc(method+" /v1/memory/{id}", func(w http.ResponseWriter, r *http.Request) {
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil || id < 1 {
				jsonReply(w, http.StatusBadRequest, map[string]string{"error": "memory ID must be a positive integer"})
				return
			}
			if r.Method == http.MethodDelete {
				err = store.Forget(r.Context(), id)
			} else {
				var item memory.Memory
				item, err = store.Inspect(r.Context(), id)
				if err == nil {
					jsonReply(w, http.StatusOK, item)
					return
				}
			}
			if err != nil {
				status := apiErrorStatus(err, http.StatusInternalServerError)
				if errors.Is(err, memory.ErrNotFound) {
					status = http.StatusNotFound
				}
				jsonReply(w, status, map[string]string{"error": err.Error()})
				return
			}
			jsonReply(w, http.StatusOK, map[string]int64{"forgotten": id})
		})
	}
	mux.HandleFunc("GET /v1/memories", func(w http.ResponseWriter, r *http.Request) {
		limit, offset := 20, 0
		var err error
		if value := r.URL.Query().Get("limit"); value != "" {
			limit, err = strconv.Atoi(value)
		}
		if err == nil {
			if value := r.URL.Query().Get("offset"); value != "" {
				offset, err = strconv.Atoi(value)
			}
		}
		if err != nil || limit < 1 || limit > 100 || offset < 0 {
			jsonReply(w, http.StatusBadRequest, map[string]string{"error": "limit must be 1..100 and offset must be nonnegative"})
			return
		}
		items, err := store.List(r.Context(), limit, offset)
		if err != nil {
			jsonReply(w, apiErrorStatus(err, http.StatusInternalServerError), map[string]string{"error": err.Error()})
			return
		}
		jsonReply(w, http.StatusOK, items)
	})
}
