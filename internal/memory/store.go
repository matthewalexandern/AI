// Package memory persists conversations and searchable memories in SQLite.
package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

const (
	maxContentBytes = 128 * 1024
	maxQueryBytes   = 4096
	maxSessionBytes = 256
	maxDetailBytes  = 16 * 1024
	maxRecallIDs    = 100
	maxSearchLimit  = 100
	maxHistoryLimit = 1000
)

// ErrInvalidInput identifies invalid memory content, filters, or turn fields.
// Storage failures do not wrap this error.
var ErrInvalidInput = errors.New("memory: invalid input")

// Message is a message suitable for passing to a chat model.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Memory is an explicit note or a completed conversation episode. A higher
// search Score means a better FTS5 match; scores are only comparable within a
// single search.
type Memory struct {
	ID        int64     `json:"id"`
	Content   string    `json:"content"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
	Score     float64   `json:"score"`
}

// Turn records a completed response and the controller's assessment of it.
type Turn struct {
	Session    string    `json:"session"`
	User       string    `json:"user"`
	Answer     string    `json:"answer"`
	Plan       string    `json:"plan"`
	Reflection string    `json:"reflection"`
	Cognition  string    `json:"cognition,omitempty"`
	Model      string    `json:"model"`
	Confidence float64   `json:"confidence"`
	RecallIDs  []int64   `json:"recall_ids"`
	CreatedAt  time.Time `json:"created_at"`
}

// Store owns the persistent database. It is safe for concurrent use. One
// connection keeps transaction order consistent across platforms, including
// an in-memory database. Connection settings are reapplied on reconnection.
type Store struct {
	db *sql.DB
}

// Open opens or creates a memory database. New databases and their newly
// created parent directories are private to the current user. The special path
// ":memory:" is useful for ephemeral stores.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("memory database path is empty")
	}
	if path != ":memory:" {
		var err error
		path, err = filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve memory database path: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, fmt.Errorf("create memory database directory: %w", err)
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			if err := file.Close(); err != nil {
				return nil, fmt.Errorf("close new memory database: %w", err)
			}
		} else if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create memory database: %w", err)
		}
	}
	dsn := path
	if path != ":memory:" {
		uriPath := filepath.ToSlash(path)
		if !strings.HasPrefix(uriPath, "/") {
			// SQLite file URIs use /C:/... for absolute Windows drive paths.
			uriPath = "/" + uriPath
		}
		dsn = (&url.URL{Scheme: "file", Path: uriPath}).String()
	}
	// These settings belong to each connection, rather than the database.
	// Encoding the path separately keeps filename characters such as ? and %
	// from being interpreted as driver options or referring to another file.
	params := url.Values{"_pragma": {"busy_timeout(5000)", "foreign_keys(ON)"}}
	dsn += "?" + params.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open memory database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.initialize(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) initialize(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
		return fmt.Errorf("configure memory database: %w", err)
	}
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read memory schema version: %w", err)
	}
	if version > 2 {
		return fmt.Errorf("memory database schema version %d is newer than supported version 2", version)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin memory schema setup: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("initialize memory schema: %w", err)
	}
	if version == 1 {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE turns ADD COLUMN cognition TEXT NOT NULL DEFAULT ''"); err != nil {
			return fmt.Errorf("migrate cognition metadata: %w", err)
		}
	}
	if version == 0 {
		if _, err := tx.ExecContext(ctx, "INSERT INTO memories_fts(memories_fts) VALUES ('rebuild')"); err != nil {
			return fmt.Errorf("initialize memory search index: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
		return fmt.Errorf("set memory schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit memory schema setup: %w", err)
	}
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS turns (
    id INTEGER PRIMARY KEY,
    session TEXT NOT NULL,
    user_content TEXT NOT NULL,
    answer_content TEXT NOT NULL,
    plan TEXT NOT NULL,
    reflection TEXT NOT NULL,
    cognition TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL,
    confidence REAL NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    recall_ids TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS turns_session_id ON turns(session, id);
CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY,
    session TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
    content TEXT NOT NULL,
    turn_id INTEGER NOT NULL REFERENCES turns(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_session_id ON messages(session, id);
CREATE TABLE IF NOT EXISTS memories (
    id INTEGER PRIMARY KEY,
    content TEXT NOT NULL,
    kind TEXT NOT NULL,
    turn_id INTEGER REFERENCES turns(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS memory_identity (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    last_id INTEGER NOT NULL CHECK (last_id >= 0)
);
INSERT INTO memory_identity(singleton, last_id)
VALUES (1, (SELECT COALESCE(MAX(id), 0) FROM memories))
ON CONFLICT(singleton) DO UPDATE SET last_id = MAX(last_id, excluded.last_id);
CREATE TRIGGER IF NOT EXISTS memories_identity AFTER INSERT ON memories BEGIN
    UPDATE memory_identity SET last_id = MAX(last_id, new.id) WHERE singleton = 1;
END;
CREATE TRIGGER IF NOT EXISTS memories_identity_update AFTER UPDATE OF id ON memories BEGIN
    UPDATE memory_identity SET last_id = MAX(last_id, new.id) WHERE singleton = 1;
END;
CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
    content,
    content='memories',
    content_rowid='id',
    tokenize='unicode61 remove_diacritics 2'
);
CREATE TRIGGER IF NOT EXISTS memories_insert AFTER INSERT ON memories BEGIN
    INSERT INTO memories_fts(rowid, content) VALUES (new.id, new.content);
END;
CREATE TRIGGER IF NOT EXISTS memories_delete AFTER DELETE ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES ('delete', old.id, old.content);
END;
CREATE TRIGGER IF NOT EXISTS memories_update AFTER UPDATE OF content ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, content) VALUES ('delete', old.id, old.content);
    INSERT INTO memories_fts(rowid, content) VALUES (new.id, new.content);
END;
`

// Close flushes and closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// Remember saves an explicit memory. Empty kinds default to "note".
func (s *Store) Remember(ctx context.Context, content, kind string) (int64, error) {
	if err := validateRequired("memory content", content, maxContentBytes); err != nil {
		return 0, err
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "note"
	}
	if err := validateRequired("memory kind", kind, 64); err != nil {
		return 0, err
	}
	result, err := s.db.ExecContext(ctx,
		"INSERT INTO memories(id, content, kind, created_at) VALUES ((SELECT last_id + 1 FROM memory_identity WHERE singleton = 1), ?, ?, ?)",
		content, kind, formatTime(time.Now()))
	if err != nil {
		return 0, fmt.Errorf("save memory: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read saved memory ID: %w", err)
	}
	return id, nil
}

// Search treats the query as ordinary text, never as caller-supplied FTS5
// syntax. It searches up to 32 distinct words with OR and ranks matches using
// BM25. A zero limit defaults to 8; larger limits are capped at 100.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]Memory, error) {
	memories := make([]Memory, 0)
	if err := validateText("memory search query", query); err != nil {
		return nil, err
	}
	if len(query) > maxQueryBytes {
		return nil, fmt.Errorf("%w: search query exceeds %d bytes", ErrInvalidInput, maxQueryBytes)
	}
	limit, err := boundedLimit(limit, 8, maxSearchLimit)
	if err != nil {
		return nil, err
	}
	match := searchExpression(query)
	if match == "" {
		return memories, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT m.id, m.content, m.kind, m.created_at, bm25(memories_fts)
FROM memories_fts JOIN memories AS m ON m.id = memories_fts.rowid
WHERE memories_fts MATCH ?
ORDER BY bm25(memories_fts), m.id DESC LIMIT ?`, match, limit)
	if err != nil {
		return nil, fmt.Errorf("search memories: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var memory Memory
		var timestamp string
		var rank float64
		if err := rows.Scan(&memory.ID, &memory.Content, &memory.Kind, &timestamp, &rank); err != nil {
			return nil, fmt.Errorf("read memory search result: %w", err)
		}
		memory.CreatedAt, err = time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			return nil, fmt.Errorf("read memory creation time: %w", err)
		}
		memory.Score = -rank
		memories = append(memories, memory)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memory search results: %w", err)
	}
	return memories, nil
}

func searchExpression(query string) string {
	words := strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r)
	})
	terms := make([]string, 0, 32)
	seen := make(map[string]bool)
	for _, word := range words {
		key := strings.ToLower(word)
		if seen[key] {
			continue
		}
		seen[key] = true
		terms = append(terms, `"`+strings.ReplaceAll(word, `"`, `""`)+`"`)
		if len(terms) == 32 {
			break
		}
	}
	return strings.Join(terms, " OR ")
}

// Messages returns the most recent messages in chronological order for one
// session. A zero limit defaults to 20; larger limits are capped at 1000.
func (s *Store) Messages(ctx context.Context, session string, limit int) ([]Message, error) {
	if err := validateRequired("session", session, maxSessionBytes); err != nil {
		return nil, err
	}
	limit, err := boundedLimit(limit, 20, maxHistoryLimit)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT role, content FROM (
    SELECT id, role, content FROM messages WHERE session = ? ORDER BY id DESC LIMIT ?
) ORDER BY id`, session, limit)
	if err != nil {
		return nil, fmt.Errorf("read conversation history: %w", err)
	}
	defer rows.Close()
	messages := make([]Message, 0)
	for rows.Next() {
		var message Message
		if err := rows.Scan(&message.Role, &message.Content); err != nil {
			return nil, fmt.Errorf("read conversation message: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate conversation history: %w", err)
	}
	return messages, nil
}

// SaveTurn commits a complete user/assistant exchange, its assessment, and a
// searchable episodic memory atomically. A zero timestamp uses the current time.
func (s *Store) SaveTurn(ctx context.Context, turn Turn) error {
	if turn.CreatedAt.IsZero() {
		turn.CreatedAt = time.Now()
	}
	if err := validateTurn(turn); err != nil {
		return err
	}
	timestamp := formatTime(turn.CreatedAt)
	recallIDs, err := json.Marshal(turn.RecallIDs)
	if err != nil {
		return fmt.Errorf("encode recalled memory IDs: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin turn transaction: %w", err)
	}
	defer tx.Rollback()
	for _, id := range turn.RecallIDs {
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM memories WHERE id = ?", id).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: recalled memory %d no longer exists", ErrInvalidInput, id)
			}
			return fmt.Errorf("check recalled memory: %w", err)
		}
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO turns(session, user_content, answer_content, plan, reflection, cognition, model, confidence, recall_ids, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, turn.Session, turn.User, turn.Answer, turn.Plan,
		turn.Reflection, turn.Cognition, turn.Model, turn.Confidence, string(recallIDs), timestamp)
	if err != nil {
		return fmt.Errorf("save turn assessment: %w", err)
	}
	turnID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("read saved turn ID: %w", err)
	}
	for _, message := range []Message{{Role: "user", Content: turn.User}, {Role: "assistant", Content: turn.Answer}} {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO messages(session, role, content, turn_id, created_at) VALUES (?, ?, ?, ?, ?)",
			turn.Session, message.Role, message.Content, turnID, timestamp); err != nil {
			return fmt.Errorf("save conversation message: %w", err)
		}
	}
	episode := "User: " + turn.User + "\nAssistant: " + turn.Answer
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO memories(id, content, kind, turn_id, created_at) VALUES ((SELECT last_id + 1 FROM memory_identity WHERE singleton = 1), ?, 'episode', ?, ?)",
		episode, turnID, timestamp); err != nil {
		return fmt.Errorf("save episodic memory: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit completed turn: %w", err)
	}
	return nil
}

func validateTurn(turn Turn) error {
	for _, field := range []struct {
		name  string
		value string
		max   int
	}{
		{"session", turn.Session, maxSessionBytes},
		{"user message", turn.User, maxContentBytes},
		{"assistant answer", turn.Answer, maxContentBytes},
	} {
		if err := validateRequired(field.name, field.value, field.max); err != nil {
			return err
		}
	}
	if len(turn.Plan) > maxDetailBytes || len(turn.Reflection) > maxDetailBytes || len(turn.Cognition) > maxDetailBytes {
		return fmt.Errorf("%w: turn plan, reflection, and cognition must each fit within %d bytes", ErrInvalidInput, maxDetailBytes)
	}
	if turn.Cognition != "" && !json.Valid([]byte(turn.Cognition)) {
		return fmt.Errorf("%w: turn cognition must be valid JSON", ErrInvalidInput)
	}
	if len(turn.Model) > 512 {
		return fmt.Errorf("%w: turn model exceeds 512 bytes", ErrInvalidInput)
	}
	for _, field := range []struct{ name, value string }{
		{"turn plan", turn.Plan}, {"turn reflection", turn.Reflection}, {"turn cognition", turn.Cognition}, {"turn model", turn.Model},
	} {
		if err := validateText(field.name, field.value); err != nil {
			return err
		}
	}
	if math.IsNaN(turn.Confidence) || math.IsInf(turn.Confidence, 0) || turn.Confidence < 0 || turn.Confidence > 1 {
		return fmt.Errorf("%w: turn confidence must be a finite number between 0 and 1", ErrInvalidInput)
	}
	if len(turn.RecallIDs) > maxRecallIDs {
		return fmt.Errorf("%w: turn has more than %d recalled memory IDs", ErrInvalidInput, maxRecallIDs)
	}
	seenIDs := make(map[int64]bool, len(turn.RecallIDs))
	for _, id := range turn.RecallIDs {
		if id <= 0 {
			return fmt.Errorf("%w: recalled memory IDs must be positive", ErrInvalidInput)
		}
		if seenIDs[id] {
			return fmt.Errorf("%w: recalled memory IDs must be distinct", ErrInvalidInput)
		}
		seenIDs[id] = true
	}
	timestamp := formatTime(turn.CreatedAt)
	if _, err := time.Parse(time.RFC3339Nano, timestamp); err != nil {
		return fmt.Errorf("%w: turn creation time is outside the supported range: %v", ErrInvalidInput, err)
	}
	return nil
}

// Episodes returns the most recent assessments in chronological order for one
// session. A zero limit defaults to 20; larger limits are capped at 1000.
func (s *Store) Episodes(ctx context.Context, session string, limit int) ([]Turn, error) {
	if err := validateRequired("session", session, maxSessionBytes); err != nil {
		return nil, err
	}
	limit, err := boundedLimit(limit, 20, maxHistoryLimit)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT session, user_content, answer_content, plan, reflection, cognition, model, confidence, recall_ids, created_at FROM (
    SELECT * FROM turns WHERE session = ? ORDER BY id DESC LIMIT ?
) ORDER BY id`, session, limit)
	if err != nil {
		return nil, fmt.Errorf("read turn assessments: %w", err)
	}
	defer rows.Close()
	turns := make([]Turn, 0)
	for rows.Next() {
		var turn Turn
		var recallIDs, timestamp string
		if err := rows.Scan(&turn.Session, &turn.User, &turn.Answer, &turn.Plan, &turn.Reflection, &turn.Cognition,
			&turn.Model, &turn.Confidence, &recallIDs, &timestamp); err != nil {
			return nil, fmt.Errorf("read turn assessment: %w", err)
		}
		if err := json.Unmarshal([]byte(recallIDs), &turn.RecallIDs); err != nil {
			return nil, fmt.Errorf("decode recalled memory IDs: %w", err)
		}
		turn.CreatedAt, err = time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			return nil, fmt.Errorf("read turn creation time: %w", err)
		}
		turns = append(turns, turn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate turn assessments: %w", err)
	}
	return turns, nil
}

func validateRequired(name, value string, maxBytes int) error {
	if err := validateText(name, value); err != nil {
		return err
	}
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: %s is empty", ErrInvalidInput, name)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalidInput, name, maxBytes)
	}
	return nil
}

func validateText(name, value string) error {
	if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%w: %s must be UTF-8 without NUL characters", ErrInvalidInput, name)
	}
	return nil
}

func boundedLimit(limit, defaultLimit, maxLimit int) (int, error) {
	if limit < 0 {
		return 0, fmt.Errorf("%w: limit must not be negative", ErrInvalidInput)
	}
	if limit == 0 {
		return defaultLimit, nil
	}
	if limit > maxLimit {
		return maxLimit, nil
	}
	return limit, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
