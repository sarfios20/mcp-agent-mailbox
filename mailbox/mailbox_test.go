package mailbox

import (
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "mailbox.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, name := range []string{"alice", "bob", "carol"} {
		if err := store.AddParticipant(name, "key-"+name); err != nil {
			t.Fatalf("AddParticipant: %v", err)
		}
	}

	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store
}

// Catches reporting unread after marking receipts, or leaving received messages unread.
func TestReadThreadMarksOnlyReceivedMessages(t *testing.T) {
	store := openTestStore(t)
	threadID, err := store.Send("alice", []string{"bob"}, "status", "first message")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := store.Reply("bob", threadID, "reply"); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	thread, err := store.ReadThread("alice", threadID)
	if err != nil {
		t.Fatalf("ReadThread: %v", err)
	}
	if got := len(thread.Messages); got != 2 {
		t.Fatalf("len(Messages) = %d, want 2", got)
	}
	if thread.Messages[0].Unread {
		t.Error("message sent by reader was reported unread")
	}
	if !thread.Messages[1].Unread {
		t.Error("received message was not reported unread before marking read")
	}
	thread, err = store.ReadThread("alice", threadID)
	if err != nil {
		t.Fatalf("second ReadThread: %v", err)
	}
	if thread.Messages[1].Unread {
		t.Error("message received by reader remains unread")
	}
}

// Catches Inbox including a thread when its only unread message was sent by the caller.
func TestInboxExcludesUnreadMessageSentByMe(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.Send("alice", []string{"bob"}, "status", "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	inbox, err := store.Inbox("alice")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(inbox) != 0 {
		t.Fatalf("len(Inbox) = %d, want 0", len(inbox))
	}
}

// Catches someone outside the thread being able to read or reply to it.
func TestNonParticipantCannotReadOrReply(t *testing.T) {
	store := openTestStore(t)
	threadID, err := store.Send("alice", []string{"bob"}, "status", "hello")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if _, err := store.ReadThread("carol", threadID); err == nil {
		t.Error("ReadThread by non-participant succeeded")
	}
	if _, err := store.Reply("carol", threadID, "intrusion"); err == nil {
		t.Error("Reply by non-participant succeeded")
	}
}

// Catches invalid recipient lists being accepted or wildcard delivery omitting a participant.
func TestSendRecipients(t *testing.T) {
	store := openTestStore(t)
	for _, test := range []struct {
		name string
		to   []string
	}{
		{"empty", []string{}},
		{"duplicate", []string{"bob", "bob"}},
		{"sender", []string{"alice"}},
		{"unknown", []string{"unknown"}},
		{"mixed wildcard", []string{"*", "bob"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.Send("alice", test.to, "status", "hello"); err == nil {
				t.Errorf("Send to %q succeeded", test.to)
			}
		})
	}
	threadID, err := store.Send("alice", []string{"*"}, "status", "hello all")
	if err != nil {
		t.Fatalf("Send to wildcard: %v", err)
	}
	for _, name := range []string{"bob", "carol"} {
		inbox, err := store.Inbox(name)
		if err != nil {
			t.Fatalf("Inbox(%q): %v", name, err)
		}
		if len(inbox) != 1 || inbox[0].ThreadID != threadID || inbox[0].Unread != 1 {
			t.Fatalf("Inbox(%q) = %+v, want wildcard thread with one unread", name, inbox)
		}
		if got := inbox[0].Participants; len(got) != 3 || got[0] != "alice" || got[1] != "bob" || got[2] != "carol" {
			t.Fatalf("participants = %q, want [alice bob carol]", got)
		}
	}
}

// Catches a group reply omitting a recipient's receipt or creating one for its sender.
func TestGroupReplyReceipts(t *testing.T) {
	store := openTestStore(t)
	threadID, err := store.Send("alice", []string{"carol", "bob"}, "status", "hello both")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	messageID, err := store.Reply("bob", threadID, "reply to both")
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	rows, err := store.db.Query("SELECT participant, read_at FROM receipts WHERE message_id = ? ORDER BY participant", messageID)
	if err != nil {
		t.Fatalf("query reply receipts: %v", err)
	}
	defer rows.Close()
	var recipients []string
	for rows.Next() {
		var name string
		var readAt any
		if err := rows.Scan(&name, &readAt); err != nil {
			t.Fatalf("scan reply receipt: %v", err)
		}
		if readAt != nil {
			t.Errorf("reply receipt for %q was already read", name)
		}
		recipients = append(recipients, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list reply receipts: %v", err)
	}
	if len(recipients) != 2 || recipients[0] != "alice" || recipients[1] != "carol" {
		t.Fatalf("reply receipts = %q, want [alice carol]", recipients)
	}
}

// Catches a late member seeing old messages as unread or missing unread receipts for new ones.
func TestAddToThreadLateMemberReceipts(t *testing.T) {
	store := openTestStore(t)
	threadID, err := store.Send("alice", []string{"bob"}, "status", "hello bob")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	participants, err := store.AddToThread("alice", threadID, []string{"carol"})
	if err != nil {
		t.Fatalf("AddToThread: %v", err)
	}
	if len(participants) != 3 || participants[0] != "alice" || participants[1] != "bob" || participants[2] != "carol" {
		t.Fatalf("participants = %q, want [alice bob carol]", participants)
	}
	thread, err := store.ReadThread("carol", threadID)
	if err != nil {
		t.Fatalf("ReadThread(carol): %v", err)
	}
	if len(thread.Messages) != 1 || thread.Messages[0].Content != "hello bob" || thread.Messages[0].Unread {
		t.Fatalf("old messages = %+v, want the old message with Unread false", thread.Messages)
	}
	messageID, err := store.Reply("alice", threadID, "welcome carol")
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	thread, err = store.ReadThread("carol", threadID)
	if err != nil {
		t.Fatalf("ReadThread(carol) after reply: %v", err)
	}
	if len(thread.Messages) != 2 || thread.Messages[1].ID != messageID || !thread.Messages[1].Unread {
		t.Fatalf("messages after reply = %+v, want the new reply unread", thread.Messages)
	}
}

// Catches one member reading a group thread clearing another member's unread receipts.
func TestGroupReadPreservesOtherMembersUnread(t *testing.T) {
	store := openTestStore(t)
	threadID, err := store.Send("alice", []string{"bob", "carol"}, "status", "hello both")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	thread, err := store.ReadThread("bob", threadID)
	if err != nil {
		t.Fatalf("ReadThread(bob): %v", err)
	}
	if len(thread.Participants) != 3 || thread.Participants[0] != "alice" || thread.Participants[1] != "bob" || thread.Participants[2] != "carol" {
		t.Fatalf("participants = %q, want [alice bob carol]", thread.Participants)
	}
	for _, name := range []string{"bob", "carol"} {
		inbox, err := store.Inbox(name)
		if err != nil {
			t.Fatalf("Inbox(%q): %v", name, err)
		}
		if name == "bob" && len(inbox) != 0 {
			t.Fatalf("bob's inbox = %+v, want no unread threads", inbox)
		}
		if name == "carol" && (len(inbox) != 1 || inbox[0].ThreadID != threadID || inbox[0].Unread != 1) {
			t.Fatalf("carol's inbox = %+v, want one unread message", inbox)
		}
	}
}
