// Package buzon stores and retrieves two-participant message threads.
package buzon

import (
	"database/sql"
	"fmt"
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
	Recipient string
	Content   string
	CreatedAt time.Time
	ReadAt    *time.Time
}

// Thread is a conversation and its messages, ordered from oldest to newest.
type Thread struct {
	ID       int64
	Subject  string
	With     string
	Messages []Message
}

// InboxEntry summarizes a thread containing unread messages for a participant.
type InboxEntry struct {
	ThreadID int64
	Subject  string
	With     string
	Unread   int
	LastAt   time.Time
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
    a TEXT,
    b TEXT,
    created_at TEXT
);
CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY,
    thread_id INTEGER REFERENCES threads,
    sender TEXT,
    recipient TEXT,
    content TEXT,
    created_at TEXT,
    read_at TEXT NULL
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

// Send creates a thread and its first unread message from me to to.
func (s *Store) Send(me, to, subject, text string) (threadID int64, err error) {
	if err := s.validateParticipant(me); err != nil {
		return 0, err
	}
	if err := s.validateParticipant(to); err != nil {
		return 0, err
	}
	if me == to {
		return 0, fmt.Errorf("recipient %q is the sender %q", to, me)
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
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	now := time.Now().UTC().Format(timeLayout)
	result, err := tx.Exec(
		"INSERT INTO threads(subject, a, b, created_at) VALUES (?, ?, ?, ?)",
		subject, me, to, now,
	)
	if err != nil {
		return 0, fmt.Errorf("create thread from %q to %q with subject %q: %w", me, to, subject, err)
	}
	threadID, err = result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("get id for thread from %q to %q: %w", me, to, err)
	}
	if _, err = tx.Exec(
		"INSERT INTO messages(thread_id, sender, recipient, content, created_at) VALUES (?, ?, ?, ?, ?)",
		threadID, me, to, text, now,
	); err != nil {
		return 0, fmt.Errorf("create message in thread %d from %q to %q: %w", threadID, me, to, err)
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit thread %d from %q to %q: %w", threadID, me, to, err)
	}
	return threadID, nil
}

// Reply appends an unread message addressed to the other thread participant.
func (s *Store) Reply(me string, threadID int64, text string) (messageID int64, err error) {
	if err := s.validateParticipant(me); err != nil {
		return 0, err
	}
	if strings.TrimSpace(text) == "" {
		return 0, fmt.Errorf("text %q is empty for thread %d", text, threadID)
	}

	var a, b string
	if err := s.db.QueryRow("SELECT a, b FROM threads WHERE id = ?", threadID).Scan(&a, &b); err != nil {
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("thread %d does not exist", threadID)
		}
		return 0, fmt.Errorf("look up thread %d for %q: %w", threadID, me, err)
	}
	recipient, err := otherParticipant(me, threadID, a, b)
	if err != nil {
		return 0, err
	}

	result, err := s.db.Exec(
		"INSERT INTO messages(thread_id, sender, recipient, content, created_at) VALUES (?, ?, ?, ?, ?)",
		threadID, me, recipient, text, time.Now().UTC().Format(timeLayout),
	)
	if err != nil {
		return 0, fmt.Errorf("reply to thread %d as %q: %w", threadID, me, err)
	}
	messageID, err = result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("get message id for thread %d as %q: %w", threadID, me, err)
	}
	return messageID, nil
}

// ReadThread returns a participant's thread and marks only messages received by
// that participant as read.
func (s *Store) ReadThread(me string, threadID int64) (thread Thread, err error) {
	if err := s.validateParticipant(me); err != nil {
		return Thread{}, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return Thread{}, fmt.Errorf("begin reading thread %d as %q: %w", threadID, me, err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var a, b string
	thread.ID = threadID
	if err = tx.QueryRow("SELECT subject, a, b FROM threads WHERE id = ?", threadID).Scan(&thread.Subject, &a, &b); err != nil {
		if err == sql.ErrNoRows {
			return Thread{}, fmt.Errorf("thread %d does not exist", threadID)
		}
		return Thread{}, fmt.Errorf("look up thread %d for %q: %w", threadID, me, err)
	}
	thread.With, err = otherParticipant(me, threadID, a, b)
	if err != nil {
		return Thread{}, err
	}

	readAt := time.Now().UTC().Format(timeLayout)
	if _, err = tx.Exec(
		"UPDATE messages SET read_at = ? WHERE thread_id = ? AND recipient = ? AND read_at IS NULL",
		readAt, threadID, me,
	); err != nil {
		return Thread{}, fmt.Errorf("mark thread %d read for %q: %w", threadID, me, err)
	}

	rows, err := tx.Query(`
SELECT id, sender, recipient, content, created_at, read_at
FROM messages
WHERE thread_id = ?
ORDER BY id`, threadID)
	if err != nil {
		return Thread{}, fmt.Errorf("list messages in thread %d for %q: %w", threadID, me, err)
	}
	defer rows.Close()
	for rows.Next() {
		var message Message
		var createdAt string
		var readAt sql.NullString
		if err = rows.Scan(
			&message.ID, &message.Sender, &message.Recipient, &message.Content, &createdAt, &readAt,
		); err != nil {
			return Thread{}, fmt.Errorf("scan message in thread %d for %q: %w", threadID, me, err)
		}
		message.CreatedAt, err = parseTime(createdAt, "created_at", message.ID)
		if err != nil {
			return Thread{}, err
		}
		if readAt.Valid {
			parsed, parseErr := parseTime(readAt.String, "read_at", message.ID)
			if parseErr != nil {
				return Thread{}, parseErr
			}
			message.ReadAt = &parsed
		}
		thread.Messages = append(thread.Messages, message)
	}
	if err = rows.Err(); err != nil {
		return Thread{}, fmt.Errorf("list messages in thread %d for %q: %w", threadID, me, err)
	}
	if err = rows.Close(); err != nil {
		return Thread{}, fmt.Errorf("close messages in thread %d for %q: %w", threadID, me, err)
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

	rows, err := s.db.Query(`
SELECT t.id, t.subject,
       CASE WHEN t.a = ? THEN t.b ELSE t.a END,
       COUNT(unread.id),
       (SELECT MAX(message.created_at)
          FROM messages AS message
         WHERE message.thread_id = t.id)
FROM threads AS t
JOIN messages AS unread
  ON unread.thread_id = t.id
 AND unread.recipient = ?
 AND unread.read_at IS NULL
WHERE t.a = ? OR t.b = ?
GROUP BY t.id, t.subject, t.a, t.b
ORDER BY 5 DESC, t.id DESC`, me, me, me, me)
	if err != nil {
		return nil, fmt.Errorf("list inbox for %q: %w", me, err)
	}
	defer rows.Close()

	var entries []InboxEntry
	for rows.Next() {
		var entry InboxEntry
		var lastAt string
		if err := rows.Scan(&entry.ThreadID, &entry.Subject, &entry.With, &entry.Unread, &lastAt); err != nil {
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
	return entries, nil
}

func (s *Store) validateParticipant(name string) error {
	if _, ok := s.participants[name]; !ok {
		return fmt.Errorf("participant %q is unknown", name)
	}
	return nil
}

func otherParticipant(me string, threadID int64, a, b string) (string, error) {
	switch me {
	case a:
		return b, nil
	case b:
		return a, nil
	default:
		return "", fmt.Errorf("participant %q does not belong to thread %d", me, threadID)
	}
}

func parseTime(value, field string, id int64) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse %s %q for record %d: %w", field, value, id, err)
	}
	return parsed, nil
}
