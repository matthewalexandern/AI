package memory

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ErrNotFound means the requested memory no longer exists.
var ErrNotFound = errors.New("memory: record not found")

const (
	snapshotFormat     = "mini-fabrics-memory"
	snapshotVersion    = 1
	maxSnapshotBytes   = 64 << 20
	maxBackupBytes     = 512 << 20
	maxSnapshotRecords = 100000
)

type snapshot struct {
	Format       string            `json:"format"`
	Version      int               `json:"version"`
	CreatedAt    time.Time         `json:"created_at"`
	LastMemoryID int64             `json:"last_memory_id"`
	Memories     []snapshotMemory  `json:"memories"`
	Turns        []snapshotTurn    `json:"turns"`
	Messages     []snapshotMessage `json:"messages"`
}

type snapshotMemory struct {
	ID        int64     `json:"id"`
	Content   string    `json:"content"`
	Kind      string    `json:"kind"`
	TurnID    *int64    `json:"turn_id"`
	CreatedAt time.Time `json:"created_at"`
}

type snapshotTurn struct {
	ID int64 `json:"id"`
	Turn
}

type snapshotMessage struct {
	ID        int64     `json:"id"`
	Session   string    `json:"session"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	TurnID    int64     `json:"turn_id"`
	CreatedAt time.Time `json:"created_at"`
}

// Inspect returns the complete stored content of a single memory.
func (s *Store) Inspect(ctx context.Context, id int64) (Memory, error) {
	if id <= 0 {
		return Memory{}, fmt.Errorf("%w: memory ID must be positive", ErrInvalidInput)
	}
	var record Memory
	var timestamp string
	err := s.db.QueryRowContext(ctx, "SELECT id, content, kind, created_at FROM memories WHERE id = ?", id).Scan(&record.ID, &record.Content, &record.Kind, &timestamp)
	if errors.Is(err, sql.ErrNoRows) {
		return Memory{}, ErrNotFound
	}
	if err != nil {
		return Memory{}, fmt.Errorf("inspect memory: %w", err)
	}
	record.CreatedAt, err = time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return Memory{}, fmt.Errorf("read memory creation time: %w", err)
	}
	return record, nil
}

// List returns newest IDs first. A zero limit defaults to 20; the maximum is 100.
func (s *Store) List(ctx context.Context, limit, offset int) ([]Memory, error) {
	limit, err := boundedLimit(limit, 20, maxSearchLimit)
	if err != nil {
		return nil, err
	}
	if offset < 0 {
		return nil, fmt.Errorf("%w: offset must not be negative", ErrInvalidInput)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id, content, kind, created_at FROM memories ORDER BY id DESC LIMIT ? OFFSET ?", limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list memories: %w", err)
	}
	defer rows.Close()
	result := make([]Memory, 0)
	for rows.Next() {
		var record Memory
		var timestamp string
		if err := rows.Scan(&record.ID, &record.Content, &record.Kind, &timestamp); err != nil {
			return nil, err
		}
		record.CreatedAt, err = time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

// Forget deletes a note or an entire episodic turn, including its history and
// FTS entry, in one transaction. Surviving turns lose references to deleted IDs.
// This is logical deletion, not secure erasure of historical disk pages/backups.
func (s *Store) Forget(ctx context.Context, id int64) error {
	if id <= 0 {
		return fmt.Errorf("%w: memory ID must be positive", ErrInvalidInput)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin forget transaction: %w", err)
	}
	defer tx.Rollback()
	var turnID sql.NullInt64
	err = tx.QueryRowContext(ctx, "SELECT turn_id FROM memories WHERE id = ?", id).Scan(&turnID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("find memory to forget: %w", err)
	}
	removed := []int64{id}
	if turnID.Valid {
		rows, err := tx.QueryContext(ctx, "SELECT id FROM memories WHERE turn_id = ?", turnID.Int64)
		if err != nil {
			return err
		}
		removed = nil
		for rows.Next() {
			var deletedID int64
			if err := rows.Scan(&deletedID); err != nil {
				rows.Close()
				return err
			}
			removed = append(removed, deletedID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		_, err = tx.ExecContext(ctx, "DELETE FROM turns WHERE id = ?", turnID.Int64)
	} else {
		_, err = tx.ExecContext(ctx, "DELETE FROM memories WHERE id = ?", id)
	}
	if err != nil {
		return fmt.Errorf("delete memory and history: %w", err)
	}
	for _, deletedID := range removed {
		_, err := tx.ExecContext(ctx, `UPDATE turns SET recall_ids = (
SELECT json_group_array(value) FROM json_each(turns.recall_ids) WHERE value != ?
) WHERE EXISTS (SELECT 1 FROM json_each(turns.recall_ids) WHERE value = ?)`, deletedID, deletedID)
		if err != nil {
			return fmt.Errorf("remove forgotten provenance: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit forgotten memory: %w", err)
	}
	return nil
}

// Export writes a versioned logical snapshot containing notes, complete turns,
// messages, and recall IDs. It reads one consistent SQLite transaction. Logical
// snapshots are limited to 64 MiB and 100,000 records per table.
func (s *Store) Export(ctx context.Context, writer io.Writer) error {
	if writer == nil {
		return fmt.Errorf("%w: export writer is required", ErrInvalidInput)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin export: %w", err)
	}
	defer tx.Rollback()
	data, err := readSnapshot(ctx, tx, 2)
	if err != nil {
		return fmt.Errorf("read export: %w", err)
	}
	if err := validateSnapshot(data); err != nil {
		return fmt.Errorf("export inconsistent database: %w", err)
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode export: %w", err)
	}
	if len(encoded)+1 > maxSnapshotBytes {
		return fmt.Errorf("%w: logical snapshot exceeds 64 MiB; use backup", ErrInvalidInput)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	_, err = io.Copy(writer, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("write export: %w", err)
	}
	return ctx.Err()
}

// Backup writes a consistent standalone SQLite database, including committed
// WAL data. The destination must not exist; it is created with mode 0600.
func (s *Store) Backup(ctx context.Context, path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%w: backup path is empty", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("reserve backup destination: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return err
	}
	complete := false
	defer func() {
		if !complete {
			os.Remove(path)
		}
	}()
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("create consistent backup: %w", err)
	}
	file, err = os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}

// Restore accepts a logical JSON export or a standalone SQLite backup. Merge
// (replace=false) appends all imported records and remaps their IDs/provenance;
// importing the same snapshot twice intentionally creates two copies. Replace
// atomically replaces current content and preserves imported IDs. Existing
// database files are never renamed or swapped while open. JSON is capped at
// 64 MiB, SQLite input at 512 MiB, and each table at 100,000 records.
func (s *Store) Restore(ctx context.Context, reader io.Reader, replace bool) error {
	if reader == nil {
		return fmt.Errorf("%w: restore reader is required", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	input := bufio.NewReader(&contextReader{ctx: ctx, reader: reader})
	header, _ := input.Peek(16)
	var data *snapshot
	var err error
	if string(header) == "SQLite format 3\x00" {
		data, err = readSQLiteSnapshot(ctx, input)
	} else {
		data, err = readJSONSnapshot(input)
	}
	if err != nil {
		return err
	}
	if err := validateSnapshot(data); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin restore: %w", err)
	}
	defer tx.Rollback()
	if replace {
		if _, err := tx.ExecContext(ctx, "DELETE FROM messages; DELETE FROM memories; DELETE FROM turns;"); err != nil {
			return fmt.Errorf("clear replaced memories: %w", err)
		}
	}
	turnIDs, err := mapIDs(ctx, tx, "turns", len(data.Turns), replace, func(i int) int64 { return data.Turns[i].ID })
	if err != nil {
		return err
	}
	memoryIDs, err := mapIDs(ctx, tx, "memories", len(data.Memories), replace, func(i int) int64 { return data.Memories[i].ID })
	if err != nil {
		return err
	}
	messageIDs, err := mapIDs(ctx, tx, "messages", len(data.Messages), replace, func(i int) int64 { return data.Messages[i].ID })
	if err != nil {
		return err
	}
	for _, record := range data.Turns {
		recalled := make([]int64, 0, len(record.RecallIDs))
		for _, id := range record.RecallIDs {
			recalled = append(recalled, memoryIDs[id])
		}
		encoded, _ := json.Marshal(recalled)
		_, err := tx.ExecContext(ctx, `INSERT INTO turns(id, session, user_content, answer_content, plan, reflection, cognition, model, confidence, recall_ids, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, turnIDs[record.ID], record.Session, record.User, record.Answer, record.Plan, record.Reflection, record.Cognition, record.Model, record.Confidence, string(encoded), formatTime(record.CreatedAt))
		if err != nil {
			return fmt.Errorf("restore turn: %w", err)
		}
	}
	for _, record := range data.Memories {
		var turnID any
		if record.TurnID != nil {
			turnID = turnIDs[*record.TurnID]
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO memories(id, content, kind, turn_id, created_at) VALUES (?, ?, ?, ?, ?)", memoryIDs[record.ID], record.Content, record.Kind, turnID, formatTime(record.CreatedAt))
		if err != nil {
			return fmt.Errorf("restore memory: %w", err)
		}
	}
	for _, record := range data.Messages {
		_, err := tx.ExecContext(ctx, "INSERT INTO messages(id, session, role, content, turn_id, created_at) VALUES (?, ?, ?, ?, ?, ?)", messageIDs[record.ID], record.Session, record.Role, record.Content, turnIDs[record.TurnID], formatTime(record.CreatedAt))
		if err != nil {
			return fmt.Errorf("restore history: %w", err)
		}
	}
	if replace {
		if _, err := tx.ExecContext(ctx, "UPDATE memory_identity SET last_id = MAX(last_id, ?) WHERE singleton = 1", data.LastMemoryID); err != nil {
			return fmt.Errorf("restore memory identity: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit restore: %w", err)
	}
	return nil
}

func mapIDs(ctx context.Context, tx *sql.Tx, table string, count int, preserve bool, original func(int) int64) (map[int64]int64, error) {
	var last int64
	if !preserve {
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0) FROM "+table).Scan(&last); err != nil {
			return nil, err
		}
		if table == "memories" {
			if err := tx.QueryRowContext(ctx, "SELECT MAX(last_id, ?) FROM memory_identity WHERE singleton = 1", last).Scan(&last); err != nil {
				return nil, err
			}
		}
		if last > int64(^uint64(0)>>1)-int64(count) {
			return nil, fmt.Errorf("%w: no available %s IDs", ErrInvalidInput, table)
		}
	}
	result := make(map[int64]int64, count)
	for i := 0; i < count; i++ {
		id := original(i)
		if preserve {
			result[id] = id
		} else {
			last++
			result[id] = last
		}
	}
	return result, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func readJSONSnapshot(reader io.Reader) (*snapshot, error) {
	encoded, err := io.ReadAll(io.LimitReader(reader, maxSnapshotBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read restore data: %w", err)
	}
	if len(encoded) > maxSnapshotBytes {
		return nil, fmt.Errorf("%w: logical snapshot exceeds 64 MiB", ErrInvalidInput)
	}
	if !utf8.Valid(encoded) {
		return nil, fmt.Errorf("%w: restore JSON is not UTF-8", ErrInvalidInput)
	}
	if err := uniqueJSONKeys(json.NewDecoder(bytes.NewReader(encoded)), 0); err != nil {
		return nil, fmt.Errorf("%w: invalid restore JSON: %v", ErrInvalidInput, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var data snapshot
	if err := decoder.Decode(&data); err != nil {
		return nil, fmt.Errorf("%w: invalid restore JSON: %v", ErrInvalidInput, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("%w: restore JSON contains trailing data", ErrInvalidInput)
	}
	return &data, nil
}

// Reject ambiguous duplicate fields before decoding into the snapshot structs.
// A small depth bound also prevents deeply nested unrelated input from consuming
// an unbounded stack before unknown fields can be rejected.
func uniqueJSONKeys(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("JSON nesting exceeds 32 levels")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, nested := token.(json.Delim)
	if !nested {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			// Match encoding/json's case-insensitive struct field matching,
			// including Unicode simple-fold equivalents.
			name = strings.Map(func(r rune) rune {
				folded := r
				for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
					if next < folded {
						folded = next
					}
				}
				return folded
			}, name)
			if !ok || seen[name] {
				return errors.New("duplicate or invalid JSON field")
			}
			seen[name] = true
			if err := uniqueJSONKeys(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := uniqueJSONKeys(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected closing JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}

func readSQLiteSnapshot(ctx context.Context, reader io.Reader) (*snapshot, error) {
	file, err := os.CreateTemp("", "mini-fabrics-restore-*.sqlite")
	if err != nil {
		return nil, err
	}
	path := file.Name()
	defer os.Remove(path)
	count, err := io.Copy(file, io.LimitReader(reader, maxBackupBytes+1))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, fmt.Errorf("stage SQLite backup: %w", err)
	}
	if count > maxBackupBytes {
		return nil, fmt.Errorf("%w: SQLite snapshot exceeds 512 MiB", ErrInvalidInput)
	}
	uriPath := filepath.ToSlash(path)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	dsn := (&url.URL{Scheme: "file", Path: uriPath, RawQuery: "mode=ro&immutable=1"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("%w: open SQLite backup: %v", ErrInvalidInput, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return nil, fmt.Errorf("%w: read backup schema: %v", ErrInvalidInput, err)
	}
	if version < 1 || version > 2 {
		return nil, fmt.Errorf("%w: unsupported backup schema %d", ErrInvalidInput, version)
	}
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
		return nil, fmt.Errorf("%w: SQLite backup integrity check failed", ErrInvalidInput)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	data, err := readSnapshot(ctx, tx, version)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: invalid SQLite backup content: %v", ErrInvalidInput, err)
	}
	return data, nil
}

func readSnapshot(ctx context.Context, tx *sql.Tx, schemaVersion int) (*snapshot, error) {
	data := &snapshot{Format: snapshotFormat, Version: snapshotVersion, CreatedAt: time.Now().UTC(), Memories: []snapshotMemory{}, Turns: []snapshotTurn{}, Messages: []snapshotMessage{}}
	var hasIdentity int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'memory_identity'").Scan(&hasIdentity); err != nil {
		return nil, err
	}
	if hasIdentity != 0 {
		if err := tx.QueryRowContext(ctx, "SELECT last_id FROM memory_identity WHERE singleton = 1").Scan(&data.LastMemoryID); err != nil {
			return nil, err
		}
	}
	var contentBytes int
	checkSize := func(size int) error {
		contentBytes += size
		if contentBytes > maxSnapshotBytes {
			return fmt.Errorf("%w: logical content exceeds 64 MiB", ErrInvalidInput)
		}
		return nil
	}
	cognition := "cognition"
	if schemaVersion == 1 {
		cognition = "''"
	}
	rows, err := tx.QueryContext(ctx, "SELECT id, session, user_content, answer_content, plan, reflection, "+cognition+", model, confidence, recall_ids, created_at FROM turns ORDER BY id LIMIT ?", maxSnapshotRecords+1)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var record snapshotTurn
		var recalled, timestamp string
		if err := rows.Scan(&record.ID, &record.Session, &record.User, &record.Answer, &record.Plan, &record.Reflection, &record.Cognition, &record.Model, &record.Confidence, &recalled, &timestamp); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(recalled), &record.RecallIDs); err != nil {
			rows.Close()
			return nil, err
		}
		record.CreatedAt, err = time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if err := checkSize(len(record.Session) + len(record.User) + len(record.Answer) + len(record.Plan) + len(record.Reflection) + len(record.Cognition) + len(record.Model) + len(recalled)); err != nil {
			rows.Close()
			return nil, err
		}
		data.Turns = append(data.Turns, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	rows, err = tx.QueryContext(ctx, "SELECT id, content, kind, turn_id, created_at FROM memories ORDER BY id LIMIT ?", maxSnapshotRecords+1)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var record snapshotMemory
		var turnID sql.NullInt64
		var timestamp string
		if err := rows.Scan(&record.ID, &record.Content, &record.Kind, &turnID, &timestamp); err != nil {
			rows.Close()
			return nil, err
		}
		if turnID.Valid {
			id := turnID.Int64
			record.TurnID = &id
		}
		record.CreatedAt, err = time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if err := checkSize(len(record.Content) + len(record.Kind)); err != nil {
			rows.Close()
			return nil, err
		}
		data.Memories = append(data.Memories, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	rows, err = tx.QueryContext(ctx, "SELECT id, session, role, content, turn_id, created_at FROM messages ORDER BY id LIMIT ?", maxSnapshotRecords+1)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var record snapshotMessage
		var timestamp string
		if err := rows.Scan(&record.ID, &record.Session, &record.Role, &record.Content, &record.TurnID, &timestamp); err != nil {
			rows.Close()
			return nil, err
		}
		record.CreatedAt, err = time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if err := checkSize(len(record.Session) + len(record.Content)); err != nil {
			rows.Close()
			return nil, err
		}
		data.Messages = append(data.Messages, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return data, nil
}

func validateSnapshot(data *snapshot) error {
	invalid := func(message string) error { return fmt.Errorf("%w: %s", ErrInvalidInput, message) }
	if data.Format != snapshotFormat || data.Version != snapshotVersion {
		return invalid("unsupported snapshot format or version")
	}
	if data.CreatedAt.IsZero() {
		return invalid("snapshot creation time is required")
	}
	if data.Memories == nil || data.Turns == nil || data.Messages == nil {
		return invalid("snapshot must include memories, turns, and messages arrays")
	}
	if len(data.Memories) > maxSnapshotRecords || len(data.Turns) > maxSnapshotRecords || len(data.Messages) > maxSnapshotRecords {
		return invalid("snapshot exceeds 100000 records per table")
	}
	sort.Slice(data.Memories, func(i, j int) bool { return data.Memories[i].ID < data.Memories[j].ID })
	sort.Slice(data.Turns, func(i, j int) bool { return data.Turns[i].ID < data.Turns[j].ID })
	sort.Slice(data.Messages, func(i, j int) bool { return data.Messages[i].ID < data.Messages[j].ID })
	turns := make(map[int64]snapshotTurn, len(data.Turns))
	for _, record := range data.Turns {
		if record.ID <= 0 || turns[record.ID].ID != 0 {
			return invalid("invalid or duplicate turn ID")
		}
		if record.CreatedAt.IsZero() {
			return invalid("turn creation time is required")
		}
		if err := validateTurn(record.Turn); err != nil {
			return err
		}
		turns[record.ID] = record
	}
	memories := make(map[int64]bool, len(data.Memories))
	episodes := make(map[int64]bool, len(data.Turns))
	for _, record := range data.Memories {
		if record.ID <= 0 || memories[record.ID] {
			return invalid("invalid or duplicate memory ID")
		}
		memories[record.ID] = true
		if record.CreatedAt.IsZero() {
			return invalid("memory creation time is required")
		}
		if err := validateRequired("memory kind", record.Kind, 64); err != nil {
			return err
		}
		limit := maxContentBytes
		if record.TurnID != nil {
			turn, ok := turns[*record.TurnID]
			if !ok || episodes[*record.TurnID] {
				return invalid("episode references a missing or duplicate turn")
			}
			if record.Kind != "episode" || record.Content != "User: "+turn.User+"\nAssistant: "+turn.Answer || !record.CreatedAt.Equal(turn.CreatedAt) {
				return invalid("episode content does not match its turn")
			}
			episodes[*record.TurnID] = true
			limit = 2*maxContentBytes + len("User: \nAssistant: ")
		}
		if err := validateRequired("memory content", record.Content, limit); err != nil {
			return err
		}
	}
	var largestMemoryID int64
	if len(data.Memories) != 0 {
		largestMemoryID = data.Memories[len(data.Memories)-1].ID
	}
	if data.LastMemoryID == 0 {
		data.LastMemoryID = largestMemoryID
	}
	if data.LastMemoryID < 0 || data.LastMemoryID < largestMemoryID {
		return invalid("snapshot memory identity is below its existing IDs")
	}
	for _, record := range data.Turns {
		if !episodes[record.ID] {
			return invalid("turn is missing its episodic memory")
		}
		seen := make(map[int64]bool)
		for _, id := range record.RecallIDs {
			if !memories[id] || seen[id] {
				return invalid("turn has missing or duplicate recall provenance")
			}
			seen[id] = true
		}
	}
	if len(data.Messages) != 2*len(data.Turns) {
		return invalid("every turn must have exactly two messages")
	}
	var lastID int64
	for i, turn := range data.Turns {
		for j, role := range []string{"user", "assistant"} {
			record := data.Messages[2*i+j]
			content := turn.User
			if j == 1 {
				content = turn.Answer
			}
			if record.ID <= lastID || record.TurnID != turn.ID || record.Session != turn.Session || record.Role != role || record.Content != content || !record.CreatedAt.Equal(turn.CreatedAt) {
				return invalid("history messages do not match complete ordered turns")
			}
			lastID = record.ID
		}
	}
	return nil
}
