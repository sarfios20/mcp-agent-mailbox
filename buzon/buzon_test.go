package buzon

import (
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "buzon.db"), []string{"ana", "bea", "carlos"})
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

// Atrapa que leer marque mensajes enviados por quien lee o deje sin marcar los recibidos.
func TestReadThreadMarksOnlyReceivedMessages(t *testing.T) {
	store := openTestStore(t)
	threadID, err := store.Send("ana", "bea", "estado", "primer mensaje")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := store.Reply("bea", threadID, "respuesta"); err != nil {
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

// Atrapa que Inbox incluya un hilo cuando el único mensaje no leído lo envió quien consulta.
func TestInboxExcludesUnreadMessageSentByMe(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.Send("ana", "bea", "estado", "hola"); err != nil {
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

// Atrapa que alguien ajeno al hilo pueda leerlo o responder en él.
func TestNonParticipantCannotReadOrReply(t *testing.T) {
	store := openTestStore(t)
	threadID, err := store.Send("ana", "bea", "estado", "hola")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if _, err := store.ReadThread("carlos", threadID); err == nil {
		t.Error("ReadThread by non-participant succeeded")
	}
	if _, err := store.Reply("carlos", threadID, "intrusión"); err == nil {
		t.Error("Reply by non-participant succeeded")
	}
}

// Atrapa que Send permita enviarse a uno mismo o escribir a un destinatario desconocido.
func TestSendRejectsSelfAndUnknownRecipient(t *testing.T) {
	store := openTestStore(t)

	if _, err := store.Send("ana", "ana", "estado", "hola"); err == nil {
		t.Error("Send to self succeeded")
	}
	if _, err := store.Send("ana", "desconocido", "estado", "hola"); err == nil {
		t.Error("Send to unknown recipient succeeded")
	}
}
