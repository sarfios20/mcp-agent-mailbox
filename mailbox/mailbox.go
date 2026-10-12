// Package mailbox stores and retrieves message threads with per-participant receipts.
package mailbox

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// timeLayout is RFC 3339 with fixed-width nanoseconds, so stored timestamps
// sort correctly as text (RFC3339Nano drops trailing zeros and does not).
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// Store is a SQLite-backed mailbox with persisted participants.
type Store struct {
	db       *sql.DB
	notifyMu sync.Mutex
	notify   chan struct{}
}

// Message is one message in a thread.
type Message struct {
	ID        int64
	Sender    string
	Content   string
	CreatedAt time.Time
	Unread    bool
}

// Thread is a conversation and its messages, ordered from oldest to newest.
type Thread struct {
	ID           int64
	Subject      string
	Participants []string
	Messages     []Message
}

// InboxEntry summarizes a thread containing unread messages for a participant.
type InboxEntry struct {
	ThreadID     int64
	Subject      string
	Participants []string
	Unread       int
	LastAt       time.Time
}

// Open opens a mailbox database and creates its schema when necessary.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database %q: %w", path, err)
	}
	// PRAGMA foreign_keys is connection-local, so keep this database on the
	// connection configured below.
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("configure database %q with %q: %w", path, pragma, err)
		}
	}

	const schema = `
CREATE TABLE IF NOT EXISTS participants (
 name TEXT PRIMARY KEY,
 public_key TEXT NOT NULL UNIQUE,
 added_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS threads (
    id INTEGER PRIMARY KEY,
    subject TEXT,
    created_by TEXT,
    created_at TEXT
);
CREATE TABLE IF NOT EXISTS thread_members (
    thread_id INTEGER REFERENCES threads,
    participant TEXT,
    added_at TEXT,
    PRIMARY KEY (thread_id, participant)
);
CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY,
    thread_id INTEGER REFERENCES threads,
    sender TEXT,
    content TEXT,
    created_at TEXT
);
CREATE TABLE IF NOT EXISTS receipts (
    message_id INTEGER REFERENCES messages,
    participant TEXT,
    read_at TEXT NULL,
    PRIMARY KEY (message_id, participant)
);`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema in database %q: %w", path, err)
	}

	return &Store{db: db, notify: make(chan struct{})}, nil
}

// Close releases the database used by the store.
func (s *Store) Close() error {
	return s.db.Close()
}

// Participants returns every currently configured participant, sorted by name.
func (s *Store) Participants() ([]string, error) {
	rows, err := s.db.Query("SELECT name FROM participants ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// AddParticipant registers a name and key, accepting an identical registration.
func (s *Store) AddParticipant(name, publicKey string) error {
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`).MatchString(name) {
		return fmt.Errorf("invalid participant name %q", name)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var stored string
	err = tx.QueryRow("SELECT public_key FROM participants WHERE name = ?", name).Scan(&stored)
	if err == nil {
		if stored != publicKey {
			return fmt.Errorf("participant %q has another public key", name)
		}
		return tx.Commit()
	}
	if err != sql.ErrNoRows {
		return err
	}
	var owner string
	err = tx.QueryRow("SELECT name FROM participants WHERE public_key = ?", publicKey).Scan(&owner)
	if err == nil {
		return fmt.Errorf("public key belongs to participant %q", owner)
	}
	if err != sql.ErrNoRows {
		return err
	}
	if _, err := tx.Exec("INSERT INTO participants(name, public_key, added_at) VALUES (?, ?, ?)", name, publicKey, time.Now().UTC().Format(timeLayout)); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveParticipant revokes a registration without deleting its thread history.
func (s *Store) RemoveParticipant(name string) (bool, error) {
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`).MatchString(name) {
		return false, fmt.Errorf("invalid participant name %q", name)
	}
	result, err := s.db.Exec("DELETE FROM participants WHERE name = ?", name)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n != 0, err
}

// ParticipantByKey looks up the current owner of a normalized public key.
func (s *Store) ParticipantByKey(publicKey string) (string, bool, error) {
	var name string
	err := s.db.QueryRow("SELECT name FROM participants WHERE public_key = ?", publicKey).Scan(&name)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return name, err == nil, err
}

// Send creates a thread with me and the recipients, expanding ["*"] at send time.
func (s *Store) Send(me string, to []string, subject, text string) (threadID int64, err error) {
	if err := s.validateParticipant(me); err != nil {
		return 0, err
	}
	if len(to) == 1 && to[0] == "*" {
		names, err := s.Participants()
		if err != nil {
			return 0, err
		}
		to = make([]string, 0)
		for _, name := range names {
			if name != me {
				to = append(to, name)
			}
		}
	}
	if len(to) == 0 {
		return 0, fmt.Errorf("recipients %q are empty", to)
	}
	seen := make(map[string]struct{}, len(to))
	for _, name := range to {
		if name == "*" {
			return 0, fmt.Errorf("recipient %q must be the only recipient in %q", name, to)
		}
		if _, duplicate := seen[name]; duplicate {
			return 0, fmt.Errorf("recipient %q is duplicated", name)
		}
		seen[name] = struct{}{}
		if name == me {
			return 0, fmt.Errorf("recipient %q is the sender %q", name, me)
		}
		if err := s.validateParticipant(name); err != nil {
			return 0, err
		}
	}
	if strings.TrimSpace(subject) == "" {
		return 0, fmt.Errorf("subject %q is empty", subject)
	}
	if strings.TrimSpace(text) == "" {
		return 0, fmt.Errorf("text %q is empty", text)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin sending from %q to %q: %w", me, to, err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(timeLayout)
	result, err := tx.Exec("INSERT INTO threads(subject, created_by, created_at) VALUES (?, ?, ?)", subject, me, now)
	if err != nil {
		return 0, fmt.Errorf("create thread from %q to %q with subject %q: %w", me, to, subject, err)
	}
	threadID, err = result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("get id for thread from %q to %q: %w", me, to, err)
	}
	members := append([]string{me}, to...)
	for _, name := range members {
		if _, err = tx.Exec("INSERT INTO thread_members(thread_id, participant, added_at) VALUES (?, ?, ?)", threadID, name, now); err != nil {
			return 0, fmt.Errorf("add participant %q to thread %d: %w", name, threadID, err)
		}
	}
	if _, err = insertMessage(tx, me, threadID, text, now); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit thread %d from %q to %q: %w", threadID, me, to, err)
	}
	s.notifyUnread()
	return threadID, nil
}

// Reply appends a message with unread receipts for every other thread member.
func (s *Store) Reply(me string, threadID int64, text string) (messageID int64, err error) {
	if err := s.validateParticipant(me); err != nil {
		return 0, err
	}
	if strings.TrimSpace(text) == "" {
		return 0, fmt.Errorf("text %q is empty for thread %d", text, threadID)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin replying to thread %d as %q: %w", threadID, me, err)
	}
	defer tx.Rollback()
	members, err := threadParticipants(tx, threadID)
	if err != nil {
		return 0, err
	}
	if err := requireMember(me, threadID, members); err != nil {
		return 0, err
	}
	messageID, err = insertMessage(tx, me, threadID, text, time.Now().UTC().Format(timeLayout))
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit reply to thread %d as %q: %w", threadID, me, err)
	}
	s.notifyUnread()
	return messageID, nil
}

// WaitUnread returns the unread receipt count, waiting for committed messages
// when there are none, until ctx is cancelled. It does not mark receipts read.
func (s *Store) WaitUnread(ctx context.Context, me string) (int, error) {
	if err := s.validateParticipant(me); err != nil {
		return 0, err
	}
	for {
		s.notifyMu.Lock()
		notify := s.notify
		s.notifyMu.Unlock()

		var unread int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM receipts WHERE participant = ? AND read_at IS NULL", me).Scan(&unread); err != nil {
			return 0, fmt.Errorf("count unread receipts for %q: %w", me, err)
		}
		if unread > 0 {
			return unread, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-notify:
		}
	}
}

// notifyUnread wakes every waiter after a Send or Reply transaction commits.
func (s *Store) notifyUnread() {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	close(s.notify)
	s.notify = make(chan struct{})
}

// AddToThread lets a member add named participants and returns all members sorted.
// New members see the history but receive receipts only for future messages.
func (s *Store) AddToThread(me string, threadID int64, names []string) ([]string, error) {
	if err := s.validateParticipant(me); err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin adding participants to thread %d as %q: %w", threadID, me, err)
	}
	defer tx.Rollback()
	members, err := threadParticipants(tx, threadID)
	if err != nil {
		return nil, err
	}
	if err := requireMember(me, threadID, members); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("participants %q are empty for thread %d", names, threadID)
	}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "*" {
			return nil, fmt.Errorf("participant %q is not supported when adding to thread %d", name, threadID)
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("participant %q is duplicated", name)
		}
		seen[name] = struct{}{}
		if err := validateParticipant(tx, name); err != nil {
			return nil, err
		}
		for _, member := range members {
			if name == member {
				return nil, fmt.Errorf("participant %q already belongs to thread %d", name, threadID)
			}
		}
	}
	now := time.Now().UTC().Format(timeLayout)
	for _, name := range names {
		if _, err := tx.Exec("INSERT INTO thread_members(thread_id, participant, added_at) VALUES (?, ?, ?)", threadID, name, now); err != nil {
			return nil, fmt.Errorf("add participant %q to thread %d: %w", name, threadID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit adding participants to thread %d as %q: %w", threadID, me, err)
	}
	members = append(members, names...)
	sort.Strings(members)
	return members, nil
}

// insertMessage inserts a message and its recipients' receipts in the caller's transaction.
func insertMessage(tx *sql.Tx, me string, threadID int64, text, now string) (int64, error) {
	result, err := tx.Exec("INSERT INTO messages(thread_id, sender, content, created_at) VALUES (?, ?, ?, ?)", threadID, me, text, now)
	if err != nil {
		return 0, fmt.Errorf("create message in thread %d from %q: %w", threadID, me, err)
	}
	messageID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("get message id for thread %d as %q: %w", threadID, me, err)
	}
	if _, err := tx.Exec(`INSERT INTO receipts(message_id, participant)
SELECT ?, participant FROM thread_members WHERE thread_id = ? AND participant != ?`, messageID, threadID, me); err != nil {
		return 0, fmt.Errorf("create receipts for message %d from %q: %w", messageID, me, err)
	}
	return messageID, nil
}

// ReadThread returns a member's thread with Unread reflecting the state before
// this call, then marks only that member's receipts read.
func (s *Store) ReadThread(me string, threadID int64) (thread Thread, err error) {
	if err := s.validateParticipant(me); err != nil {
		return Thread{}, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return Thread{}, fmt.Errorf("begin reading thread %d as %q: %w", threadID, me, err)
	}
	defer tx.Rollback()
	thread.ID = threadID
	if err = tx.QueryRow("SELECT subject FROM threads WHERE id = ?", threadID).Scan(&thread.Subject); err != nil {
		if err == sql.ErrNoRows {
			return Thread{}, fmt.Errorf("thread %d does not exist", threadID)
		}
		return Thread{}, fmt.Errorf("look up thread %d for %q: %w", threadID, me, err)
	}
	thread.Participants, err = threadParticipants(tx, threadID)
	if err != nil {
		return Thread{}, err
	}
	if err = requireMember(me, threadID, thread.Participants); err != nil {
		return Thread{}, err
	}

	rows, err := tx.Query(`
SELECT m.id, m.sender, m.content, m.created_at,
       r.message_id IS NOT NULL AND r.read_at IS NULL
FROM messages AS m
LEFT JOIN receipts AS r ON r.message_id = m.id AND r.participant = ?
WHERE m.thread_id = ?
ORDER BY m.id`, me, threadID)
	if err != nil {
		return Thread{}, fmt.Errorf("list messages in thread %d for %q: %w", threadID, me, err)
	}
	defer rows.Close()
	for rows.Next() {
		var message Message
		var createdAt string
		if err = rows.Scan(
			&message.ID, &message.Sender, &message.Content, &createdAt, &message.Unread,
		); err != nil {
			return Thread{}, fmt.Errorf("scan message in thread %d for %q: %w", threadID, me, err)
		}
		message.CreatedAt, err = parseTime(createdAt, "created_at", message.ID)
		if err != nil {
			return Thread{}, err
		}
		thread.Messages = append(thread.Messages, message)
	}
	if err = rows.Err(); err != nil {
		return Thread{}, fmt.Errorf("list messages in thread %d for %q: %w", threadID, me, err)
	}
	if err = rows.Close(); err != nil {
		return Thread{}, fmt.Errorf("close messages in thread %d for %q: %w", threadID, me, err)
	}
	if _, err = tx.Exec(`UPDATE receipts SET read_at = ?
WHERE participant = ? AND read_at IS NULL
  AND message_id IN (SELECT id FROM messages WHERE thread_id = ?)`, time.Now().UTC().Format(timeLayout), me, threadID); err != nil {
		return Thread{}, fmt.Errorf("mark thread %d read for %q: %w", threadID, me, err)
	}
	if err = tx.Commit(); err != nil {
		return Thread{}, fmt.Errorf("commit reading thread %d as %q: %w", threadID, me, err)
	}
	return thread, nil
}

// Inbox lists threads with messages that me has not read, newest first.
func (s *Store) Inbox(me string) ([]InboxEntry, error) {
	if err := s.validateParticipant(me); err != nil {
		return nil, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin listing inbox for %q: %w", me, err)
	}
	defer tx.Rollback()
	rows, err := tx.Query(`
SELECT t.id, t.subject, COUNT(r.message_id),
       (SELECT MAX(created_at) FROM messages WHERE thread_id = t.id)
FROM threads AS t
JOIN messages AS m ON m.thread_id = t.id
JOIN receipts AS r ON r.message_id = m.id AND r.participant = ? AND r.read_at IS NULL
GROUP BY t.id, t.subject
ORDER BY 4 DESC, t.id DESC`, me)
	if err != nil {
		return nil, fmt.Errorf("list inbox for %q: %w", me, err)
	}
	defer rows.Close()

	var entries []InboxEntry
	for rows.Next() {
		var entry InboxEntry
		var lastAt string
		if err := rows.Scan(&entry.ThreadID, &entry.Subject, &entry.Unread, &lastAt); err != nil {
			return nil, fmt.Errorf("scan inbox entry for %q: %w", me, err)
		}
		entry.LastAt, err = parseTime(lastAt, "created_at", entry.ThreadID)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list inbox for %q: %w", me, err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close inbox for %q: %w", me, err)
	}
	for i := range entries {
		entries[i].Participants, err = threadParticipants(tx, entries[i].ThreadID)
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit listing inbox for %q: %w", me, err)
	}
	return entries, nil
}

// validateParticipant checks current registration before a mailbox operation.
func (s *Store) validateParticipant(name string) error { return validateParticipant(s.db, name) }

// validateParticipant uses the caller's connection or transaction to avoid nested queries.
func validateParticipant(q interface{ QueryRow(string, ...any) *sql.Row }, name string) error {
	var found string
	err := q.QueryRow("SELECT name FROM participants WHERE name = ?", name).Scan(&found)
	if err == sql.ErrNoRows {
		return fmt.Errorf("participant %q is unknown", name)
	}
	return err
}

// threadParticipants lists the persisted members within the caller's transaction.
func threadParticipants(tx *sql.Tx, threadID int64) ([]string, error) {
	rows, err := tx.Query("SELECT participant FROM thread_members WHERE thread_id = ? ORDER BY participant", threadID)
	if err != nil {
		return nil, fmt.Errorf("list participants in thread %d: %w", threadID, err)
	}
	defer rows.Close()
	var members []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan participant in thread %d: %w", threadID, err)
		}
		members = append(members, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list participants in thread %d: %w", threadID, err)
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("thread %d does not exist", threadID)
	}
	return members, nil
}

// requireMember rejects access by anyone outside the thread.
func requireMember(me string, threadID int64, members []string) error {
	for _, name := range members {
		if name == me {
			return nil
		}
	}
	return fmt.Errorf("participant %q does not belong to thread %d", me, threadID)
}

func parseTime(value, field string, id int64) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse %s %q for record %d: %w", field, value, id, err)
	}
	return parsed, nil
}
