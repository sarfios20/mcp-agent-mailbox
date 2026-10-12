package mailbox

import (
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "mailbox.db"), []string{"ana", "bea", "carlos"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store
}

// Catches reading marking messages sent by the reader or leaving received messages unmarked.
func TestReadThreadMarksOnlyReceivedMessages(t *testing.T) {
	store := openTestStore(t)
	threadID, err := store.Send("ana", "bea", "status", "first message")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := store.Reply("bea", threadID, "reply"); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	thread, err := store.ReadThread("ana", threadID)
	if err != nil {
		t.Fatalf("ReadThread: %v", err)
	}
	if got := len(thread.Messages); got != 2 {
		t.Fatalf("len(Messages) = %d, want 2", got)
	}
	if thread.Messages[0].ReadAt != nil {
		t.Error("message sent by reader was marked read")
	}
	if thread.Messages[1].ReadAt == nil {
		t.Error("message received by reader remains unread")
	}
}

// Catches Inbox including a thread when its only unread message was sent by the caller.
func TestInboxExcludesUnreadMessageSentByMe(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.Send("ana", "bea", "status", "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	inbox, err := store.Inbox("ana")
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
	threadID, err := store.Send("ana", "bea", "status", "hello")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if _, err := store.ReadThread("carlos", threadID); err == nil {
		t.Error("ReadThread by non-participant succeeded")
	}
	if _, err := store.Reply("carlos", threadID, "intrusion"); err == nil {
		t.Error("Reply by non-participant succeeded")
	}
}

// Catches Send allowing messages to oneself or to an unknown recipient.
func TestSendRejectsSelfAndUnknownRecipient(t *testing.T) {
	store := openTestStore(t)

	if _, err := store.Send("ana", "ana", "status", "hello"); err == nil {
		t.Error("Send to self succeeded")
	}
	if _, err := store.Send("ana", "unknown", "status", "hello"); err == nil {
		t.Error("Send to unknown recipient succeeded")
	}
}
