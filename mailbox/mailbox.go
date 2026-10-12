// Package mailbox stores and retrieves message threads with per-participant receipts.
package mailbox

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// timeLayout is RFC 3339 with fixed-width nanoseconds, so stored timestamps
// sort correctly as text (RFC3339Nano drops trailing zeros and does not).
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// Store is a SQLite-backed mailbox for a fixed set of participants.
type Store struct {
	db           *sql.DB
	participants map[string]struct{}
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
func Open(path string, participants []string) (*Store, error) {
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

	known := make(map[string]struct{}, len(participants))
	for _, participant := range participants {
		known[participant] = struct{}{}
	}
	return &Store{db: db, participants: known}, nil
}

// Close releases the database used by the store.
func (s *Store) Close() error {
	return s.db.Close()
}

// Participants returns every currently configured participant, sorted by name.
func (s *Store) Participants() []string {
	names := make([]string, 0, len(s.participants))
	for name := range s.participants {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Send creates a thread with me and the recipients, expanding ["*"] at send time.
func (s *Store) Send(me string, to []string, subject, text string) (threadID int64, err error) {
	if err := s.validateParticipant(me); err != nil {
		return 0, err
	}
	if len(to) == 1 && to[0] == "*" {
		to = make([]string, 0, len(s.participants)-1)
		for _, name := range s.Participants() {
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
	return messageID, nil
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

func (s *Store) validateParticipant(name string) error {
	if _, ok := s.participants[name]; !ok {
		return fmt.Errorf("participant %q is unknown", name)
	}
	return nil
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
