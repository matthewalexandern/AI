package memory

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func lifecycleRemember(t *testing.T, store *Store, content string) int64 {
	t.Helper()
	id, err := store.Remember(context.Background(), content, "note")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func lifecycleTurn(t *testing.T, store *Store, session, user string, recall ...int64) int64 {
	t.Helper()
	turn := Turn{
		Session: session, User: user, Answer: "Answer for " + user,
		Plan: "Retrieve relevant evidence", Reflection: "No requirements missing",
		Model: "local.gguf", Confidence: .75, RecallIDs: recall,
		CreatedAt: time.Date(2026, 4, 5, 6, 7, 8, 9, time.UTC),
	}
	if err := store.SaveTurn(context.Background(), turn); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := store.db.QueryRow("SELECT m.id FROM memories m JOIN turns t ON t.id = m.turn_id WHERE t.session = ? AND t.user_content = ?", session, user).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func lifecycleExport(t *testing.T, store *Store) []byte {
	t.Helper()
	var data bytes.Buffer
	if err := store.Export(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data.Bytes()) {
		t.Fatalf("export is not valid JSON: %q", data.String())
	}
	return data.Bytes()
}

func lifecycleAssertSearch(t *testing.T, store *Store, query string, want int) []Memory {
	t.Helper()
	got, err := store.Search(context.Background(), query, 100)
	if err != nil || len(got) != want {
		t.Fatalf("search %q: got %+v, %v; want %d results", query, got, err, want)
	}
	return got
}

func TestLifecycleInspectAndList(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	first := lifecycleRemember(t, store, "first café note")
	second := lifecycleRemember(t, store, "second note")
	third := lifecycleRemember(t, store, "third note")
	got, err := store.Inspect(ctx, first)
	if err != nil || got.ID != first || got.Content != "first café note" || got.Kind != "note" || got.CreatedAt.IsZero() {
		t.Fatalf("inspect = %+v, %v", got, err)
	}
	page, err := store.List(ctx, 2, 0)
	if err != nil || len(page) != 2 || page[0].ID != third || page[1].ID != second {
		t.Fatalf("first list page = %+v, %v", page, err)
	}
	page, err = store.List(ctx, 2, 2)
	if err != nil || len(page) != 1 || page[0].ID != first {
		t.Fatalf("second list page = %+v, %v", page, err)
	}
	page, err = store.List(ctx, 2, 3)
	if err != nil || len(page) != 0 {
		t.Fatalf("past-end list = %+v, %v", page, err)
	}
	if _, err := store.Inspect(ctx, third+100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown inspect error = %v, want ErrNotFound", err)
	}
	if err := store.Forget(ctx, third+100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown forget error = %v, want ErrNotFound", err)
	}
	for _, id := range []int64{0, -1} {
		if _, err := store.Inspect(ctx, id); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("inspect(%d) error = %v", id, err)
		}
		if err := store.Forget(ctx, id); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("forget(%d) error = %v", id, err)
		}
	}
	for _, bounds := range [][2]int{{-1, 0}, {1, -1}} {
		if _, err := store.List(ctx, bounds[0], bounds[1]); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("list%v error = %v", bounds, err)
		}
	}
}

func TestLifecycleForgetPrunesHistoryFTSAndProvenance(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	note := lifecycleRemember(t, store, "forgottenorchid")
	retained := lifecycleRemember(t, store, "retainedfern")
	episode := lifecycleTurn(t, store, "conversation", "forgottenquestion", note, retained)
	lifecycleTurn(t, store, "conversation", "survivingquestion", note, episode, retained)
	if err := store.Forget(ctx, note); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Inspect(ctx, note); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted note still inspectable: %v", err)
	}
	lifecycleAssertSearch(t, store, "forgottenorchid", 0)
	turns, err := store.Episodes(ctx, "conversation", 20)
	if err != nil || len(turns) != 2 || !reflect.DeepEqual(turns[0].RecallIDs, []int64{retained}) || !reflect.DeepEqual(turns[1].RecallIDs, []int64{episode, retained}) {
		t.Fatalf("forget note did not prune provenance: %+v, %v", turns, err)
	}
	if err := store.Forget(ctx, episode); err != nil {
		t.Fatal(err)
	}
	lifecycleAssertSearch(t, store, "forgottenquestion", 0)
	lifecycleAssertSearch(t, store, "survivingquestion", 1)
	turns, err = store.Episodes(ctx, "conversation", 20)
	if err != nil || len(turns) != 1 || turns[0].User != "survivingquestion" || !reflect.DeepEqual(turns[0].RecallIDs, []int64{retained}) {
		t.Fatalf("forget episode did not remove its turn and provenance: %+v, %v", turns, err)
	}
	messages, err := store.Messages(ctx, "conversation", 20)
	want := []Message{{Role: "user", Content: "survivingquestion"}, {Role: "assistant", Content: "Answer for survivingquestion"}}
	if err != nil || !reflect.DeepEqual(messages, want) {
		t.Fatalf("forget episode retained history: %+v, %v", messages, err)
	}
	for _, query := range []string{"SELECT count(*) FROM messages WHERE turn_id NOT IN (SELECT id FROM turns)", "SELECT count(*) FROM memories WHERE turn_id IS NOT NULL AND turn_id NOT IN (SELECT id FROM turns)"} {
		var count int
		if err := store.db.QueryRow(query).Scan(&count); err != nil || count != 0 {
			t.Fatalf("orphan rows after forget: %d, %v", count, err)
		}
	}
}

func TestLifecycleRestoreMergeRemapsProvenance(t *testing.T) {
	ctx := context.Background()
	source := openTestStore(t)
	note := lifecycleRemember(t, source, "importednote")
	episode := lifecycleTurn(t, source, "imported", "importedfirst", note)
	lifecycleTurn(t, source, "imported", "importedsecond", note, episode)
	data := lifecycleExport(t, source)
	target := openTestStore(t)
	localNote := lifecycleRemember(t, target, "existingnote")
	lifecycleTurn(t, target, "existing", "existingquestion", localNote)
	if err := target.Restore(ctx, bytes.NewReader(data), false); err != nil {
		t.Fatal(err)
	}
	lifecycleAssertSearch(t, target, "existingnote", 1)
	notes := lifecycleAssertSearch(t, target, "importednote", 1)
	episodes := lifecycleAssertSearch(t, target, "importedfirst", 1)
	if notes[0].ID == localNote || notes[0].ID == note || episodes[0].ID == episode {
		t.Fatalf("merge did not remap colliding IDs: note %+v, episode %+v", notes, episodes)
	}
	turns, err := target.Episodes(ctx, "imported", 20)
	if err != nil || len(turns) != 2 || !reflect.DeepEqual(turns[0].RecallIDs, []int64{notes[0].ID}) || !reflect.DeepEqual(turns[1].RecallIDs, []int64{notes[0].ID, episodes[0].ID}) {
		t.Fatalf("merge lost provenance: %+v, %v", turns, err)
	}
	messages, err := target.Messages(ctx, "imported", 20)
	if err != nil || len(messages) != 4 || messages[0].Content != "importedfirst" || messages[2].Content != "importedsecond" {
		t.Fatalf("imported message chronology = %+v, %v", messages, err)
	}
	localTurns, err := target.Episodes(ctx, "existing", 20)
	if err != nil || len(localTurns) != 1 || !reflect.DeepEqual(localTurns[0].RecallIDs, []int64{localNote}) {
		t.Fatalf("merge changed existing provenance: %+v, %v", localTurns, err)
	}
}

func TestLifecycleRestoreReplacePersistsIDsAndHistory(t *testing.T) {
	ctx := context.Background()
	source := openTestStore(t)
	note := lifecycleRemember(t, source, "replacementnote")
	lifecycleTurn(t, source, "replacement", "replacementquestion", note)
	// Nonconsecutive IDs make preserving IDs distinguishable from reinsertion.
	if _, err := source.db.Exec("UPDATE memories SET id = 40 WHERE id = ?", note); err != nil {
		t.Fatal(err)
	}
	if _, err := source.db.Exec("UPDATE turns SET recall_ids = '[40]'"); err != nil {
		t.Fatal(err)
	}
	data := lifecycleExport(t, source)
	path := filepath.Join(t.TempDir(), "restored.sqlite")
	target, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { target.Close() }()
	local := lifecycleRemember(t, target, "discardednote")
	lifecycleTurn(t, target, "discarded", "discardedquestion", local)
	if err := target.Restore(ctx, bytes.NewReader(data), true); err != nil {
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	target, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := source.List(ctx, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := target.List(ctx, 100, 0)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened replacement memories = %+v, %v; want %+v", got, err, want)
	}
	lifecycleAssertSearch(t, target, "discardednote discardedquestion", 0)
	lifecycleAssertSearch(t, target, "replacementnote", 1)
	turns, err := target.Episodes(ctx, "replacement", 20)
	if err != nil || len(turns) != 1 || !reflect.DeepEqual(turns[0].RecallIDs, []int64{40}) {
		t.Fatalf("replacement provenance = %+v, %v", turns, err)
	}
	messages, err := target.Messages(ctx, "replacement", 20)
	if err != nil || len(messages) != 2 {
		t.Fatalf("replacement history = %+v, %v", messages, err)
	}
}

func TestLifecycleInvalidRestorePreservesOriginal(t *testing.T) {
	source := openTestStore(t)
	note := lifecycleRemember(t, source, "snapshotnote")
	lifecycleTurn(t, source, "snapshot", "snapshotquestion", note)
	valid := lifecycleExport(t, source)
	mutate := func(t *testing.T, update func(map[string]any)) []byte {
		t.Helper()
		var document map[string]any
		if err := json.Unmarshal(valid, &document); err != nil {
			t.Fatal(err)
		}
		update(document)
		data, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	cases := map[string]func(*testing.T) []byte{
		"truncated":         func(*testing.T) []byte { return valid[:len(valid)/2] },
		"trailing document": func(*testing.T) []byte { return append(append([]byte(nil), valid...), []byte("{}")...) },
		"wrong version":     func(t *testing.T) []byte { return mutate(t, func(doc map[string]any) { doc["version"] = 999 }) },
		"missing messages":  func(t *testing.T) []byte { return mutate(t, func(doc map[string]any) { delete(doc, "messages") }) },
		"duplicate memory ID": func(t *testing.T) []byte {
			return mutate(t, func(doc map[string]any) {
				memories := doc["memories"].([]any)
				memories[1].(map[string]any)["id"] = memories[0].(map[string]any)["id"]
			})
		},
		"dangling provenance": func(t *testing.T) []byte {
			return mutate(t, func(doc map[string]any) { doc["turns"].([]any)[0].(map[string]any)["recall_ids"] = []int64{999} })
		},
		"mismatched message": func(t *testing.T) []byte {
			return mutate(t, func(doc map[string]any) {
				doc["messages"].([]any)[0].(map[string]any)["content"] = "different question"
			})
		},
		"mismatched episode": func(t *testing.T) []byte {
			return mutate(t, func(doc map[string]any) { doc["memories"].([]any)[1].(map[string]any)["content"] = "different answer" })
		},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			target := openTestStore(t)
			id := lifecycleRemember(t, target, "preservednote")
			lifecycleTurn(t, target, "preserved", "preservedquestion", id)
			before, err := target.List(context.Background(), 100, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := target.Restore(context.Background(), bytes.NewReader(data(t)), true); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("invalid snapshot error = %v, want ErrInvalidInput", err)
			}
			after, err := target.List(context.Background(), 100, 0)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("invalid restore changed original memories: %+v, %v", after, err)
			}
			turns, err := target.Episodes(context.Background(), "preserved", 20)
			if err != nil || len(turns) != 1 || !reflect.DeepEqual(turns[0].RecallIDs, []int64{id}) {
				t.Fatalf("invalid restore changed original turn: %+v, %v", turns, err)
			}
			lifecycleAssertSearch(t, target, "preservednote", 1)
		})
	}
}

func TestLifecycleRestoreSQLFailureRollsBackReplacement(t *testing.T) {
	ctx := context.Background()
	source := openTestStore(t)
	lifecycleTurn(t, source, "imported", "importedquestion")
	target := openTestStore(t)
	id := lifecycleRemember(t, target, "originalnote")
	lifecycleTurn(t, target, "original", "originalquestion", id)
	if _, err := target.db.Exec("CREATE TRIGGER fail_restore BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT, 'test restore failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := target.Restore(ctx, bytes.NewReader(lifecycleExport(t, source)), true); err == nil {
		t.Fatal("restore unexpectedly succeeded through failure trigger")
	}
	lifecycleAssertSearch(t, target, "originalnote", 1)
	lifecycleAssertSearch(t, target, "originalquestion", 1)
	lifecycleAssertSearch(t, target, "importedquestion", 0)
	messages, err := target.Messages(ctx, "original", 20)
	if err != nil || len(messages) != 2 {
		t.Fatalf("failed replacement lost original history: %+v, %v", messages, err)
	}
}

func TestLifecycleBackupIncludesLiveWALAndRestores(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "live.sqlite")
	source, err := Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.db.Exec("PRAGMA wal_autocheckpoint = 0"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	note := lifecycleRemember(t, source, "livewalnote")
	lifecycleTurn(t, source, "live", "livewalquestion", note)
	info, err := os.Stat(sourcePath + "-wal")
	if err != nil || info.Size() <= 32 {
		t.Fatalf("test requires uncheckpointed WAL data: %v, %v", info, err)
	}
	backupPath := filepath.Join(dir, "backup space #100%.sqlite")
	if err := source.Backup(ctx, backupPath); err != nil {
		t.Fatal(err)
	}
	backup, err := Open(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	var integrity string
	if err := backup.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		backup.Close()
		t.Fatalf("backup integrity = %q, %v", integrity, err)
	}
	lifecycleAssertSearch(t, backup, "livewalnote", 1)
	lifecycleAssertSearch(t, backup, "livewalquestion", 1)
	lifecycleRemember(t, source, "afterbackuponly")
	lifecycleAssertSearch(t, backup, "afterbackuponly", 0)
	if err := backup.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(backupPath)
	if err != nil || !bytes.HasPrefix(data, []byte("SQLite format 3\x00")) {
		t.Fatalf("backup is not a standalone SQLite database: %v", err)
	}
	target := openTestStore(t)
	lifecycleRemember(t, target, "existingtargetnote")
	if err := target.Restore(ctx, bytes.NewReader(data), true); err != nil {
		t.Fatal(err)
	}
	lifecycleAssertSearch(t, target, "livewalnote", 1)
	lifecycleAssertSearch(t, target, "existingtargetnote afterbackuponly", 0)
	turns, err := target.Episodes(ctx, "live", 20)
	if err != nil || len(turns) != 1 || !reflect.DeepEqual(turns[0].RecallIDs, []int64{note}) {
		t.Fatalf("SQLite restore provenance = %+v, %v", turns, err)
	}
	messages, err := target.Messages(ctx, "live", 20)
	if err != nil || len(messages) != 2 {
		t.Fatalf("SQLite restore history = %+v, %v", messages, err)
	}
	if err := source.Backup(ctx, backupPath); err == nil {
		t.Fatal("backup overwrote an existing destination")
	}
	after, err := os.ReadFile(backupPath)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatalf("existing backup changed after rejected overwrite: %v", err)
	}
}

func TestLifecycleCanceledOperationsPreserveState(t *testing.T) {
	store := openTestStore(t)
	id := lifecycleRemember(t, store, "cancellationnote")
	data := lifecycleExport(t, store)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	backupPath := filepath.Join(t.TempDir(), "cancelled.sqlite")
	var output bytes.Buffer
	cases := map[string]func() error{
		"inspect": func() error { _, err := store.Inspect(ctx, id); return err },
		"list":    func() error { _, err := store.List(ctx, 10, 0); return err },
		"forget":  func() error { return store.Forget(ctx, id) },
		"export":  func() error { return store.Export(ctx, &output) },
		"restore": func() error { return store.Restore(ctx, bytes.NewReader(data), true) },
		"backup":  func() error { return store.Backup(ctx, backupPath) },
	}
	for name, operation := range cases {
		t.Run(name, func(t *testing.T) {
			if err := operation(); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled operation returned %v, want context.Canceled", err)
			}
		})
	}
	if _, err := os.Stat(backupPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled backup left an output file: %v", err)
	}
	lifecycleAssertSearch(t, store, "cancellationnote", 1)
}

type lifecycleFailWriter struct{ err error }

func (w lifecycleFailWriter) Write([]byte) (int, error) { return 0, w.err }

func TestLifecycleExportReportsWriterFailure(t *testing.T) {
	store := openTestStore(t)
	lifecycleRemember(t, store, "kept on export failure")
	want := errors.New("test writer is full")
	if err := store.Export(context.Background(), lifecycleFailWriter{want}); !errors.Is(err, want) {
		t.Fatalf("writer failure = %v, want %v", err, want)
	}
	lifecycleAssertSearch(t, store, "kept", 1)
}

func TestLifecycleRestoreReadFailurePreservesOriginal(t *testing.T) {
	store := openTestStore(t)
	lifecycleRemember(t, store, "original after truncated stream")
	want := errors.New("test reader interrupted")
	reader := io.MultiReader(strings.NewReader(`{"format":"mini-fabrics-memory",`), lifecycleFailReader{want})
	if err := store.Restore(context.Background(), reader, true); err == nil {
		t.Fatal("restore swallowed reader error")
	}
	lifecycleAssertSearch(t, store, "original", 1)
}

func TestLifecycleRestoreRejectsUnrelatedAndFutureSQLite(t *testing.T) {
	ctx := context.Background()
	for _, future := range []bool{false, true} {
		name := "unrelated database"
		if future {
			name = "future schema"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input.sqlite")
			if future {
				source, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				_, updateErr := source.db.Exec("PRAGMA user_version = 3")
				closeErr := source.Close()
				if updateErr != nil || closeErr != nil {
					t.Fatalf("create future schema: %v, %v", updateErr, closeErr)
				}
			} else {
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				_, createErr := db.Exec("CREATE TABLE unrelated (id INTEGER PRIMARY KEY, data TEXT)")
				closeErr := db.Close()
				if createErr != nil || closeErr != nil {
					t.Fatalf("create unrelated database: %v, %v", createErr, closeErr)
				}
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			target := openTestStore(t)
			id := lifecycleRemember(t, target, "original preserved")
			if err := target.Restore(ctx, bytes.NewReader(data), true); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("unsupported SQLite error = %v, want ErrInvalidInput", err)
			}
			got, err := target.Inspect(ctx, id)
			if err != nil || got.Content != "original preserved" {
				t.Fatalf("unsupported SQLite restore changed original memory: %+v, %v", got, err)
			}
		})
	}
}

type lifecycleFailReader struct{ err error }

func (r lifecycleFailReader) Read([]byte) (int, error) { return 0, r.err }

func TestLifecycleLegacySchemaMigrationAndSQLiteRestore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	source, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	note := lifecycleRemember(t, source, "legacynote")
	lifecycleTurn(t, source, "legacy", "legacyquestion", note)
	// Remove the v2-only column to reproduce the schema deployed before
	// structured cognition metadata was introduced.
	_, downgradeErr := source.db.Exec("DROP TRIGGER memories_identity; DROP TRIGGER memories_identity_update; DROP TABLE memory_identity; ALTER TABLE turns DROP COLUMN cognition; PRAGMA user_version = 1")
	closeErr := source.Close()
	if downgradeErr != nil || closeErr != nil {
		t.Fatalf("create schema 1 fixture: %v, %v", downgradeErr, closeErr)
	}
	legacy, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	restored := openTestStore(t)
	if err := restored.Restore(ctx, bytes.NewReader(legacy), true); err != nil {
		t.Fatalf("restore legacy SQLite backup: %v", err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("open and migrate legacy database: %v", err)
	}
	defer upgraded.Close()
	for name, store := range map[string]*Store{"restored": restored, "migrated": upgraded} {
		t.Run(name, func(t *testing.T) {
			var version int
			if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
				t.Fatalf("schema version = %d, %v", version, err)
			}
			turns, err := store.Episodes(ctx, "legacy", 10)
			if err != nil || len(turns) != 1 || turns[0].Cognition != "" || !reflect.DeepEqual(turns[0].RecallIDs, []int64{note}) {
				t.Fatalf("legacy turn = %+v, %v", turns, err)
			}
			messages, err := store.Messages(ctx, "legacy", 10)
			if err != nil || len(messages) != 2 || messages[0].Content != "legacyquestion" {
				t.Fatalf("legacy messages = %+v, %v", messages, err)
			}
			lifecycleAssertSearch(t, store, "legacynote", 1)
			lifecycleAssertSearch(t, store, "legacyquestion", 1)
			turn := Turn{Session: "new", User: "newquestion", Answer: "newanswer", Cognition: `{"intent":"answer","evidence":[]}`}
			if err := store.SaveTurn(ctx, turn); err != nil {
				t.Fatalf("save cognition after upgrade: %v", err)
			}
			turns, err = store.Episodes(ctx, "new", 10)
			if err != nil || len(turns) != 1 || turns[0].Cognition != turn.Cognition {
				t.Fatalf("new cognition after upgrade = %+v, %v", turns, err)
			}
			lifecycleExport(t, store)
		})
	}
}

func TestLifecycleCognitionMetadataSurvivesSnapshots(t *testing.T) {
	ctx := context.Background()
	source := openTestStore(t)
	note := lifecycleRemember(t, source, "cognitionnote")
	cognition := `{"intent":"explain","constraints":["cite evidence"],"evidence":[{"memory_id":1,"summary":"stored context"}],"uncertainties":[],"decision":"answer"}`
	turn := Turn{Session: "cognition", User: "explain the evidence", Answer: "An evidence based answer", Plan: "Inspect evidence", Reflection: "The answer is supported", Cognition: cognition, Confidence: .8, Model: "model.gguf", RecallIDs: []int64{note}, CreatedAt: time.Date(2026, 2, 3, 4, 5, 6, 7, time.UTC)}
	if err := source.SaveTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	jsonData := lifecycleExport(t, source)
	backupPath := filepath.Join(t.TempDir(), "cognition.sqlite")
	if err := source.Backup(ctx, backupPath); err != nil {
		t.Fatal(err)
	}
	sqliteData, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"JSON": jsonData, "SQLite": sqliteData} {
		t.Run(name, func(t *testing.T) {
			target := openTestStore(t)
			if err := target.Restore(ctx, bytes.NewReader(data), true); err != nil {
				t.Fatal(err)
			}
			turns, err := target.Episodes(ctx, "cognition", 10)
			if err != nil || len(turns) != 1 || !reflect.DeepEqual(turns[0], turn) {
				t.Fatalf("restored cognition turn = %+v, %v; want %+v", turns, err, turn)
			}
		})
	}
}

func TestLifecycleSaveTurnRejectsUnrestorableProvenance(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"missing", "forgotten", "duplicate"} {
		t.Run(name, func(t *testing.T) {
			store := openTestStore(t)
			id := lifecycleRemember(t, store, "retainednote")
			ids := []int64{id + 100}
			if name == "forgotten" {
				forgotten := lifecycleRemember(t, store, "forgottennote")
				if err := store.Forget(ctx, forgotten); err != nil {
					t.Fatal(err)
				}
				ids = []int64{forgotten}
			} else if name == "duplicate" {
				ids = []int64{id, id}
			}
			turn := Turn{Session: "invalid", User: "invalidquestion", Answer: "invalidanswer", RecallIDs: ids}
			if err := store.SaveTurn(ctx, turn); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("invalid provenance accepted: %v", err)
			}
			messages, err := store.Messages(ctx, "invalid", 10)
			if err != nil || len(messages) != 0 {
				t.Fatalf("invalid turn partially saved: %+v, %v", messages, err)
			}
			lifecycleAssertSearch(t, store, "invalidquestion", 0)
			lifecycleExport(t, store)
		})
	}
}

func TestLifecycleBackupProtectsExistingFilesAndPermissions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "source.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	lifecycleRemember(t, store, "retained source content")
	if err := store.Backup(ctx, path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("backup to itself error = %v, want existing-file error", err)
	}
	lifecycleAssertSearch(t, store, "retained", 1)
	existingPath := filepath.Join(dir, "existing.txt")
	original := []byte("existing user file must survive")
	if err := os.WriteFile(existingPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Backup(ctx, existingPath); !errors.Is(err, os.ErrExist) {
		t.Fatalf("backup over user file error = %v, want existing-file error", err)
	}
	after, err := os.ReadFile(existingPath)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatalf("backup changed existing user file: %q, %v", after, err)
	}
	backupPath := filepath.Join(dir, "private.sqlite")
	if err := store.Backup(ctx, backupPath); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		t.Fatalf("backup is readable by other users: mode %v", info.Mode())
	}
}

func TestLifecycleRestoreRejectsAmbiguousAndInvalidJSONText(t *testing.T) {
	ctx := context.Background()
	source := openTestStore(t)
	lifecycleRemember(t, source, "valid content")
	valid := lifecycleExport(t, source)
	cases := map[string][]byte{
		"duplicate root key":      bytes.Replace(valid, []byte(`"format":`), []byte(`"version":999,"format":`), 1),
		"duplicate record key":    bytes.Replace(valid, []byte(`"content":`), []byte(`"content":"different content","content":`), 1),
		"case variant root key":   bytes.Replace(valid, []byte(`"format":`), []byte(`"VERSION":999,"format":`), 1),
		"case variant record key": bytes.Replace(valid, []byte(`"content":`), []byte(`"CONTENT":"different content","content":`), 1),
		"invalid UTF-8":           bytes.Replace(valid, []byte("valid content"), []byte{'v', 0xff}, 1),
		"NUL content":             bytes.Replace(valid, []byte("valid content"), []byte(`bad\u0000content`), 1),
		"oversized content":       bytes.Replace(valid, []byte("valid content"), bytes.Repeat([]byte("a"), maxContentBytes+1), 1),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			target := openTestStore(t)
			id := lifecycleRemember(t, target, "preserved original")
			if err := target.Restore(ctx, bytes.NewReader(data), true); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("invalid JSON input accepted: %v", err)
			}
			got, err := target.Inspect(ctx, id)
			if err != nil || got.Content != "preserved original" {
				t.Fatalf("rejected JSON restore changed state: %+v, %v", got, err)
			}
		})
	}
}

func TestLifecycleDeletedMemoryIDsAreNotReused(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "identity.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	lifecycleRemember(t, store, "retained first memory")
	deleted := lifecycleRemember(t, store, "deleted highest memory")
	if err := store.Forget(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	next := lifecycleRemember(t, store, "memory after reopen")
	if next <= deleted {
		t.Fatalf("reopened store reused forgotten ID: got %d, last deleted %d", next, deleted)
	}
	if err := store.Forget(ctx, next); err != nil {
		t.Fatal(err)
	}
	episode := lifecycleTurn(t, store, "identity", "identityquestion")
	if episode <= next {
		t.Fatalf("episode reused forgotten ID: got %d, last deleted %d", episode, next)
	}
	if err := store.Forget(ctx, episode); err != nil {
		t.Fatal(err)
	}
	source := openTestStore(t)
	lifecycleRemember(t, source, "mergeduniquenote")
	if err := store.Restore(ctx, bytes.NewReader(lifecycleExport(t, source)), false); err != nil {
		t.Fatal(err)
	}
	imported := lifecycleAssertSearch(t, store, "mergeduniquenote", 1)
	if imported[0].ID <= episode {
		t.Fatalf("merge reused forgotten ID: got %d, last deleted %d", imported[0].ID, episode)
	}
	if err := store.Restore(ctx, bytes.NewReader(lifecycleExport(t, source)), true); err != nil {
		t.Fatal(err)
	}
	afterReplace := lifecycleRemember(t, store, "note after replacement")
	if afterReplace <= imported[0].ID {
		t.Fatalf("replacement reset identity history: got %d, previous %d", afterReplace, imported[0].ID)
	}
}

type lifecycleSpacesReader struct{ remaining int64 }

func (r *lifecycleSpacesReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = ' '
	}
	r.remaining -= int64(len(p))
	return len(p), nil
}

func TestLifecycleRestoreEnforcesJSONByteLimit(t *testing.T) {
	store := openTestStore(t)
	id := lifecycleRemember(t, store, "original bounded import")
	reader := &lifecycleSpacesReader{remaining: maxSnapshotBytes + 4096}
	err := store.Restore(context.Background(), reader, true)
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "64 MiB") {
		t.Fatalf("oversized JSON error = %v, want bounded-input rejection", err)
	}
	if reader.remaining == 0 {
		t.Fatal("restore consumed the entire oversized stream")
	}
	got, err := store.Inspect(context.Background(), id)
	if err != nil || got.Content != "original bounded import" {
		t.Fatalf("oversized restore changed original memory: %+v, %v", got, err)
	}
}

func TestLifecycleSnapshotsPreserveDeletedMemoryIdentity(t *testing.T) {
	ctx := context.Background()
	source := openTestStore(t)
	lifecycleRemember(t, source, "retained snapshot note")
	deleted := lifecycleRemember(t, source, "forgotten highest snapshot note")
	if err := source.Forget(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	jsonData := lifecycleExport(t, source)
	backupPath := filepath.Join(t.TempDir(), "identity-backup.sqlite")
	if err := source.Backup(ctx, backupPath); err != nil {
		t.Fatal(err)
	}
	sqliteData, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"JSON": jsonData, "SQLite": sqliteData} {
		t.Run(name, func(t *testing.T) {
			target := openTestStore(t)
			if err := target.Restore(ctx, bytes.NewReader(data), true); err != nil {
				t.Fatal(err)
			}
			id := lifecycleRemember(t, target, "new note after restore")
			if id <= deleted {
				t.Fatalf("snapshot restore reused a deleted source ID: got %d, last deleted %d", id, deleted)
			}
		})
	}
}

func TestLifecycleRestoreRejectsInvalidMemoryIdentity(t *testing.T) {
	ctx := context.Background()
	source := openTestStore(t)
	lifecycleRemember(t, source, "first source note")
	lifecycleRemember(t, source, "second source note")
	valid := lifecycleExport(t, source)
	for name, highwater := range map[string]int64{"negative": -1, "below maximum stored ID": 1} {
		t.Run(name, func(t *testing.T) {
			var document map[string]any
			if err := json.Unmarshal(valid, &document); err != nil {
				t.Fatal(err)
			}
			document["last_memory_id"] = highwater
			data, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			target := openTestStore(t)
			id := lifecycleRemember(t, target, "preserved destination")
			if err := target.Restore(ctx, bytes.NewReader(data), true); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("invalid identity value accepted: %v", err)
			}
			got, err := target.Inspect(ctx, id)
			if err != nil || got.Content != "preserved destination" {
				t.Fatalf("invalid identity restore changed state: %+v, %v", got, err)
			}
		})
	}
}
