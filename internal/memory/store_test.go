package memory

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func TestPersistentRecallAndTurnAssessment(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nested", "memory.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.Remember(ctx, "Orchid prefers a cooler room and indirect sunlight.", "preference")
	if err != nil {
		t.Fatal(err)
	}
	turn := Turn{
		Session: "gardening", User: "How do I water a fern?", Answer: "Keep fern soil moist.",
		Plan: "Use the remembered gardening preferences.", Reflection: "The answer needs no external tool.",
		Model: "local-model.gguf", Confidence: 0.8, RecallIDs: []int64{id},
		CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 123, time.FixedZone("test", -5*60*60)),
	}
	if err := store.SaveTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	memories, err := store.Search(ctx, "orchid", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 1 || memories[0].ID != id || memories[0].Kind != "preference" || memories[0].Score <= 0 || memories[0].CreatedAt.IsZero() {
		t.Fatalf("persisted explicit recall = %+v", memories)
	}
	memories, err = store.Search(ctx, "fern", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 1 || memories[0].Kind != "episode" || !strings.Contains(memories[0].Content, turn.Answer) {
		t.Fatalf("persisted episode recall = %+v", memories)
	}
	episodes, err := store.Episodes(ctx, turn.Session, 10)
	if err != nil {
		t.Fatal(err)
	}
	turn.CreatedAt = turn.CreatedAt.UTC()
	if len(episodes) != 1 || !reflect.DeepEqual(episodes[0], turn) {
		t.Fatalf("assessment round trip = %+v, want %+v", episodes, turn)
	}
	messages, err := store.Messages(ctx, turn.Session, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []Message{{Role: "user", Content: turn.User}, {Role: "assistant", Content: turn.Answer}}
	if !reflect.DeepEqual(messages, want) {
		t.Fatalf("persisted messages = %+v, want %+v", messages, want)
	}
}

func TestDatabasePathIsLiteral(t *testing.T) {
	filename := "memory space #100%.sqlite"
	if runtime.GOOS != "windows" {
		// A question mark is legal in Unix filenames but has meaning in a DSN.
		filename = "memory?notes #100%.sqlite"
	}
	path := filepath.Join(t.TempDir(), filename)
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remember(context.Background(), "literal filename persisted", "note"); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		t.Fatalf("requested database file was not populated: info=%v, err=%v", info, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filename {
		t.Fatalf("database opened a different filename: %v", entries)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	records, err := store.Search(context.Background(), "literal", 1)
	if err != nil || len(records) != 1 {
		t.Fatalf("literal filename did not round trip: %+v, %v", records, err)
	}
}

func TestReplacementConnectionsPreservePragmas(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "reconnect.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.SaveTurn(ctx, Turn{Session: "reconnect", User: "cascadequestion", Answer: "cascadeanswer"}); err != nil {
		t.Fatal(err)
	}
	// Force subsequent operations to use new physical connections, as can
	// also happen when database/sql discards a failed driver connection.
	store.db.SetMaxIdleConns(0)
	for name, want := range map[string]int{"foreign_keys": 1, "busy_timeout": 5000} {
		var got int
		if err := store.db.QueryRowContext(ctx, "PRAGMA "+name).Scan(&got); err != nil || got != want {
			t.Fatalf("replacement connection %s = %d, want %d: %v", name, got, want, err)
		}
	}
	if _, err := store.db.ExecContext(ctx, "DELETE FROM turns"); err != nil {
		t.Fatal(err)
	}
	messages, err := store.Messages(ctx, "reconnect", 10)
	if err != nil || len(messages) != 0 {
		t.Fatalf("foreign-key cascade did not remove history: %+v, %v", messages, err)
	}
	records, err := store.Search(ctx, "cascadequestion", 10)
	if err != nil || len(records) != 0 {
		t.Fatalf("foreign-key cascade did not remove indexed episodes: %+v, %v", records, err)
	}
}

func TestSearchQuotesOperatorsAndUnicode(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	id, err := store.Remember(ctx, `A résumé for the orchid gardener says "don't overwater".`, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remember(ctx, "Unrelated bicycles use chain lubricant.", "note"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`"orchid"`, `orchid* AND (`, `content:"orchid" NOT -}`, `NEAR(orchid)`, `don't`, `résumé`, `resume`, `"orchid"; DROP TABLE memories; --`} {
		t.Run(query, func(t *testing.T) {
			matches, err := store.Search(ctx, query, 0)
			if err != nil {
				t.Fatalf("plain text query returned a syntax error: %v", err)
			}
			if len(matches) != 1 || matches[0].ID != id || matches[0].Kind != "note" {
				t.Fatalf("quote-safe recall = %+v", matches)
			}
		})
	}
	for _, query := range []string{"", `" '* () : -`, "   "} {
		matches, err := store.Search(ctx, query, 0)
		if err != nil || len(matches) != 0 {
			t.Fatalf("empty token search %q = %+v, %v", query, matches, err)
		}
	}
}

func TestSearchIndexTracksUpdatesAndDeletes(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	id, err := store.Remember(ctx, "caterpillar", "note")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE memories SET content = ? WHERE id = ?", "butterfly", id); err != nil {
		t.Fatal(err)
	}
	old, err := store.Search(ctx, "caterpillar", 10)
	if err != nil || len(old) != 0 {
		t.Fatalf("stale index entry = %+v, %v", old, err)
	}
	updated, err := store.Search(ctx, "butterfly", 10)
	if err != nil || len(updated) != 1 || updated[0].ID != id {
		t.Fatalf("updated index entry = %+v, %v", updated, err)
	}
	if _, err := store.db.ExecContext(ctx, "DELETE FROM memories WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.Search(ctx, "butterfly", 10)
	if err != nil || len(deleted) != 0 {
		t.Fatalf("deleted index entry = %+v, %v", deleted, err)
	}
}

func TestSessionsAndRecentHistory(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	session := `one' OR 1=1 --`
	for _, turn := range []Turn{
		{Session: session, User: "first question", Answer: "first answer"},
		{Session: "other", User: "private question", Answer: "private answer"},
		{Session: session, User: "last question", Answer: "last answer"},
	} {
		if err := store.SaveTurn(ctx, turn); err != nil {
			t.Fatal(err)
		}
	}
	messages, err := store.Messages(ctx, session, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []Message{{Role: "user", Content: "last question"}, {Role: "assistant", Content: "last answer"}}
	if !reflect.DeepEqual(messages, want) {
		t.Fatalf("recent session messages = %+v, want %+v", messages, want)
	}
	episodes, err := store.Episodes(ctx, session, 1)
	if err != nil || len(episodes) != 1 || episodes[0].Answer != "last answer" {
		t.Fatalf("recent session assessment = %+v, %v", episodes, err)
	}
	all, err := store.Messages(ctx, session, 10)
	if err != nil || len(all) != 4 || all[0].Content != "first question" {
		t.Fatalf("complete session history = %+v, %v", all, err)
	}
	empty, err := store.Messages(ctx, "unknown", 10)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty session = %+v, %v", empty, err)
	}
}

func TestTurnFailureRollsBackEveryArtifact(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `
CREATE TRIGGER fail_episode BEFORE INSERT ON memories WHEN new.kind = 'episode'
BEGIN SELECT RAISE(ABORT, 'forced late write failure'); END;`); err != nil {
		t.Fatal(err)
	}
	err := store.SaveTurn(ctx, Turn{Session: "failed", User: "rollbackneedle", Answer: "rollback answer", Confidence: 0.5})
	if err == nil || !strings.Contains(err.Error(), "forced late write failure") {
		t.Fatalf("late transaction failure = %v", err)
	}
	for _, table := range []string{"turns", "messages", "memories"} {
		var count int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("partial turn leaked into %s: %d rows", table, count)
		}
	}
	matches, err := store.Search(ctx, "rollbackneedle", 10)
	if err != nil || len(matches) != 0 {
		t.Fatalf("rolled-back FTS data = %+v, %v", matches, err)
	}
	if _, err := store.db.ExecContext(ctx, "DROP TRIGGER fail_episode"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTurn(ctx, Turn{Session: "failed", User: "recoveryneedle", Answer: "recovered"}); err != nil {
		t.Fatalf("store unusable after rollback: %v", err)
	}
}

func TestInputBoundsAndCanceledWrites(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	for _, content := range []string{"", "\n \t", strings.Repeat("x", maxContentBytes+1)} {
		if _, err := store.Remember(ctx, content, "note"); err == nil {
			t.Fatalf("accepted invalid content of length %d", len(content))
		}
	}
	if _, err := store.Search(ctx, strings.Repeat("x", maxQueryBytes+1), 8); err == nil {
		t.Fatal("accepted oversized search")
	}
	if _, err := store.Search(ctx, "needle", -1); err == nil {
		t.Fatal("accepted negative limit")
	}
	for _, confidence := range []float64{-0.1, 1.1, math.NaN(), math.Inf(1)} {
		if err := store.SaveTurn(ctx, Turn{Session: "bounds", User: "question", Answer: "answer", Confidence: confidence}); err == nil {
			t.Fatalf("accepted invalid confidence %v", confidence)
		}
	}
	if err := store.SaveTurn(ctx, Turn{Session: "bounds", User: "question", Answer: "answer", RecallIDs: []int64{-1}}); err == nil {
		t.Fatal("accepted invalid recalled memory ID")
	}
	if err := store.SaveTurn(ctx, Turn{Session: "bounds", User: "question", Answer: "answer", CreatedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}); err == nil {
		t.Fatal("accepted a timestamp that cannot round trip")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.SaveTurn(canceled, Turn{Session: "bounds", User: "question", Answer: "answer"}); err == nil {
		t.Fatal("committed canceled turn")
	}
	turns, err := store.Episodes(ctx, "bounds", 10)
	if err != nil || len(turns) != 0 {
		t.Fatalf("invalid or canceled turn persisted: %+v, %v", turns, err)
	}
}

func TestInvalidTextIsRejectedBeforePersistence(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	for _, invalid := range []string{"nul\x00text", string([]byte{'b', 'a', 'd', 0xff})} {
		if _, err := store.Remember(ctx, invalid, "note"); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid content error = %v", err)
		}
		if _, err := store.Remember(ctx, "valid", invalid); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid kind error = %v", err)
		}
		if _, err := store.Search(ctx, invalid, 1); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid query error = %v", err)
		}
		for _, field := range []string{"session", "user", "answer", "plan", "reflection", "model"} {
			turn := Turn{Session: "text", User: "valid question", Answer: "valid answer"}
			switch field {
			case "session":
				turn.Session = invalid
			case "user":
				turn.User = invalid
			case "answer":
				turn.Answer = invalid
			case "plan":
				turn.Plan = invalid
			case "reflection":
				turn.Reflection = invalid
			case "model":
				turn.Model = invalid
			}
			if err := store.SaveTurn(ctx, turn); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("invalid %s error = %v", field, err)
			}
		}
	}
	turns, err := store.Episodes(ctx, "text", 20)
	if err != nil || len(turns) != 0 {
		t.Fatalf("invalid text persisted: %+v, %v", turns, err)
	}
}

func TestStorageFailuresAreNotInvalidInput(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, rememberErr := store.Remember(ctx, "valid text", "note")
	_, searchErr := store.Search(ctx, "valid", 1)
	_, historyErr := store.Messages(ctx, "valid", 1)
	_, episodesErr := store.Episodes(ctx, "valid", 1)
	for name, err := range map[string]error{"remember": rememberErr, "search": searchErr, "messages": historyErr, "episodes": episodesErr} {
		if err == nil || errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s storage failure was misclassified: %v", name, err)
		}
	}
}

func TestConcurrentTurnsRemainComplete(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	var group sync.WaitGroup
	errCh := make(chan error, 10)
	for i := 0; i < 10; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			errCh <- store.SaveTurn(ctx, Turn{Session: "parallel", User: "question", Answer: "answer"})
		}()
	}
	group.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	messages, err := store.Messages(ctx, "parallel", 100)
	if err != nil || len(messages) != 20 {
		t.Fatalf("concurrent history = %d messages, %v", len(messages), err)
	}
	for i := 0; i < len(messages); i += 2 {
		if messages[i].Role != "user" || messages[i+1].Role != "assistant" {
			t.Fatalf("interleaved turn at %d: %+v", i, messages[i:i+2])
		}
	}
	episodes, err := store.Episodes(ctx, "parallel", 100)
	if err != nil || len(episodes) != 10 {
		t.Fatalf("concurrent assessments = %d, %v", len(episodes), err)
	}
}

func TestNewerSchemaIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("PRAGMA user_version = 3"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if store, err := Open(path); err == nil {
		store.Close()
		t.Fatal("opened unsupported future schema")
	}
}
