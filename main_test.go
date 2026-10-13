package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sarfios20/mcp-agent-mailbox/mailbox"
	"golang.org/x/crypto/ssh"
)

// Catches authentication admitting a key absent from the participants table.
func TestAuthenticationRejectsUnknownKey(t *testing.T) {
	address, _, _ := startSSHTestServer(t)
	_, err := ssh.Dial("tcp", address, sshTestConfig(testSigner(t)))
	if err == nil {
		t.Fatal("unknown key was accepted")
	}
}

// Catches forbidden exec commands being accepted as MCP sessions.
func TestRejectsUnknownCommands(t *testing.T) {
	address, keys, _ := startSSHTestServer(t)
	client := dialSSHTestClient(t, address, keys["alice"])
	for _, command := range []string{"ls", "mcp extra", "wait extra", ""} {
		t.Run(command, func(t *testing.T) {
			session, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			output, err := session.CombinedOutput(command)
			if err == nil {
				t.Fatalf("command %q was accepted", command)
			}
			if !strings.Contains(string(output), "only mcp") {
				t.Fatalf("missing refusal reason: %q", output)
			}
		})
	}
}

// Catches an injected sender field impersonating the SSH key's participant.
func TestSendRejectsInjectedSender(t *testing.T) {
	address, keys, store := startSSHTestServer(t)
	alice := connectTestClient(t, address, keys["alice"])
	result, err := alice.CallTool(context.Background(), &mcp.CallToolParams{Name: "send", Arguments: map[string]any{
		"to": []string{"bob"}, "subject": "status", "text": "spoofed message", "sender": "bob",
	}})
	if err != nil {
		t.Fatalf("CallTool(send): %v", err)
	}
	if !result.IsError {
		t.Fatal("send with an injected sender field was accepted")
	}
	inbox, err := store.Inbox("bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 0 {
		t.Fatal("rejected send left a thread in the recipient inbox")
	}
}

// Catches the SSH MCP transport losing delivery or binding tools to the SSH user.
func TestSendAppearsInRecipientsInbox(t *testing.T) {
	address, keys, _ := startSSHTestServer(t)
	alice := connectTestClient(t, address, keys["alice"])
	bob := connectTestClient(t, address, keys["bob"])
	sent := callTool[sendOutput](t, alice, "send", map[string]any{"to": []string{"bob"}, "subject": "delivery", "text": "done"})
	inbox := callTool[inboxOutput](t, bob, "inbox", map[string]any{})
	if len(inbox.Threads) != 1 || inbox.Threads[0].ThreadID != sent.ThreadID {
		t.Fatalf("unexpected inbox: %+v", inbox)
	}
	if got := inbox.Threads[0].Participants; len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Fatalf("participants = %q, want [alice bob]", got)
	}
}

// startSSHTestServer serves real SSH connections against an isolated mailbox.
func startSSHTestServer(t *testing.T) (string, map[string]ssh.Signer, *mailbox.Store) {
	t.Helper()
	dataDir := t.TempDir()
	store, err := mailbox.Open(filepath.Join(dataDir, "mailbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	keys := make(map[string]ssh.Signer)
	for _, name := range []string{"alice", "bob"} {
		keys[name] = testSigner(t)
		if err := store.AddParticipant(name, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(keys[name].PublicKey())))); err != nil {
			t.Fatal(err)
		}
	}
	server, err := newSSHServer(store, dataDir, defaultPort)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); <-done })
	return listener.Addr().String(), keys, store
}

// testSigner creates a distinct authentication key for each test participant.
func testSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// sshTestConfig deliberately uses an unrelated username to test key-only identity.
func sshTestConfig(key ssh.Signer) *ssh.ClientConfig {
	return &ssh.ClientConfig{User: "ignored", Auth: []ssh.AuthMethod{ssh.PublicKeys(key)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: handshakeTimeout}
}

// dialSSHTestClient opens an authenticated connection and registers its cleanup.
func dialSSHTestClient(t *testing.T, address string, key ssh.Signer) *ssh.Client {
	t.Helper()
	client, err := ssh.Dial("tcp", address, sshTestConfig(key))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// connectTestClient starts the mcp exec channel and connects the SDK IO transport.
func connectTestClient(t *testing.T, address string, key ssh.Signer) *mcp.ClientSession {
	t.Helper()
	client := dialSSHTestClient(t, address, key)
	channel, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = channel.Close() })
	reader, err := channel.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	writer, err := channel.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := channel.Start("mcp"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	t.Cleanup(cancel)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "mailbox-test", Version: "1.0.0"}, nil).Connect(ctx, &mcp.IOTransport{Reader: io.NopCloser(reader), Writer: writer}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// callTool invokes a tool and decodes its structured output into the
// type expected by the test.
func callTool[Out any](t *testing.T, session *mcp.ClientSession, name string, arguments any) Out {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("CallTool(%q): %v", name, err)
	}
	if result.IsError {
		t.Fatalf("CallTool(%q) returned tool error: %v", name, result.GetError())
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal %q output: %v", name, err)
	}
	var out Out
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode %q output: %v", name, err)
	}
	return out
}

// Catches the handshake timeout being lost (gliderlabs clears conn deadlines on
// every read) and authenticated connections being cut by the same timer.
func TestHandshakeTimeoutClosesSilentConnectionsOnly(t *testing.T) {
	address, keys, _ := startSSHTestServer(t)
	client := dialSSHTestClient(t, address, keys["alice"])
	silent, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	_ = silent.SetReadDeadline(time.Now().Add(handshakeTimeout + 2*time.Second))
	if _, err := io.Copy(io.Discard, silent); err != nil {
		t.Fatalf("silent connection still open after %s: %v", handshakeTimeout, err)
	}
	if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatalf("authenticated connection closed by the handshake timer: %v", err)
	}
}

// Catches a session opened before remove still acting as the name after it
// was given to a new key.
func TestSessionStopsWhenItsKeyIsReplaced(t *testing.T) {
	address, keys, store := startSSHTestServer(t)
	alice := connectTestClient(t, address, keys["alice"])
	if _, err := store.RemoveParticipant("alice"); err != nil {
		t.Fatal(err)
	}
	if err := store.AddParticipant("alice", strings.TrimSpace(string(ssh.MarshalAuthorizedKey(testSigner(t).PublicKey())))); err != nil {
		t.Fatal(err)
	}
	result, err := alice.CallTool(context.Background(), &mcp.CallToolParams{Name: "send", Arguments: map[string]any{
		"to": []string{"bob"}, "subject": "old key", "text": "still here",
	}})
	if err == nil && !result.IsError {
		t.Fatal("send over a session whose key was replaced was accepted")
	}
}

// Catches a participant storing messages larger than one tool result can carry.
func TestSendAndReplyRejectOversizedText(t *testing.T) {
	address, keys, _ := startSSHTestServer(t)
	alice := connectTestClient(t, address, keys["alice"])
	sent := callTool[sendOutput](t, alice, "send", map[string]any{"to": []string{"bob"}, "subject": "s", "text": strings.Repeat("a", maxTextBytes)})
	for name, args := range map[string]map[string]any{
		"send":  {"to": []string{"bob"}, "subject": "s", "text": strings.Repeat("a", maxTextBytes+1)},
		"reply": {"thread_id": sent.ThreadID, "text": strings.Repeat("a", maxTextBytes+1)},
	} {
		result, err := alice.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err == nil && !result.IsError {
			t.Fatalf("%s accepted text over %d bytes", name, maxTextBytes)
		}
	}
}

// Catches a connection authenticated before remove opening new sessions and
// taking the session slots of the name's next key.
func TestRemovedKeyCannotOpenSessions(t *testing.T) {
	address, keys, store := startSSHTestServer(t)
	client := dialSSHTestClient(t, address, keys["alice"])
	if _, err := store.RemoveParticipant("alice"); err != nil {
		t.Fatal(err)
	}
	if session, err := client.NewSession(); err == nil {
		session.Close()
		t.Fatal("a removed key opened a session over its old connection")
	}
}

// Catches read_thread returning more than an SDK client accepts (16 MiB) and
// marking read messages the client never got: many short messages pass any
// text cap, and control characters are the worst case for JSON escaping.
func TestReadThreadPages(t *testing.T) {
	address, keys, _ := startSSHTestServer(t)
	alice := connectTestClient(t, address, keys["alice"])
	bob := connectTestClient(t, address, keys["bob"])
	extra := 50
	short := callTool[sendOutput](t, alice, "send", map[string]any{"to": []string{"bob"}, "subject": "short", "text": "a"})
	for i := 1; i < maxPageMessages+extra; i++ {
		callTool[replyOutput](t, alice, "reply", map[string]any{"thread_id": short.ThreadID, "text": "a"})
	}
	first := callTool[readThreadOutput](t, bob, "read_thread", map[string]any{"thread_id": short.ThreadID})
	if len(first.Thread.Messages) != maxPageMessages || !first.More || first.NextAfter != first.Thread.Messages[maxPageMessages-1].ID {
		t.Fatalf("first page: %d messages, more %v, next_after %d", len(first.Thread.Messages), first.More, first.NextAfter)
	}
	inbox := callTool[inboxOutput](t, bob, "inbox", map[string]any{})
	if len(inbox.Threads) != 1 || inbox.Threads[0].Unread != extra {
		t.Fatalf("after the first page the inbox is %+v, want %d unread", inbox.Threads, extra)
	}
	second := callTool[readThreadOutput](t, bob, "read_thread", map[string]any{"thread_id": short.ThreadID, "after": first.NextAfter})
	if len(second.Thread.Messages) != extra || second.More || !second.Thread.Messages[0].Unread {
		t.Fatalf("second page: %d messages, more %v", len(second.Thread.Messages), second.More)
	}
	if inbox := callTool[inboxOutput](t, bob, "inbox", map[string]any{}); len(inbox.Threads) != 0 {
		t.Fatalf("inbox not empty after reading every page: %+v", inbox.Threads)
	}

	text := strings.Repeat("\x01", maxTextBytes)
	large := callTool[sendOutput](t, alice, "send", map[string]any{"to": []string{"bob"}, "subject": "large", "text": text})
	for i := 0; i < maxPageBytes/maxTextBytes; i++ {
		callTool[replyOutput](t, alice, "reply", map[string]any{"thread_id": large.ThreadID, "text": text})
	}
	page := callTool[readThreadOutput](t, bob, "read_thread", map[string]any{"thread_id": large.ThreadID})
	if len(page.Thread.Messages) != maxPageBytes/maxTextBytes || !page.More {
		t.Fatalf("large page: %d messages, more %v", len(page.Thread.Messages), page.More)
	}

	result, err := bob.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_thread", Arguments: map[string]any{"thread_id": short.ThreadID, "after": -1}})
	if err == nil && !result.IsError {
		t.Fatal("read_thread accepted a negative after")
	}
}

// Catches unregistered keys sharing one session quota while an invite is
// pending, which lets a caller without a token hold every slot and block the
// invited key from joining.
func TestTokenlessKeysCannotBlockJoin(t *testing.T) {
	address, _, store := startSSHTestServer(t)
	token := "invite-token"
	hash := sha256.Sum256([]byte(token))
	now := time.Now()
	if err := store.CreateInvite("carol", "localhost", fmt.Sprintf("%x", hash), now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	squatter := dialSSHTestClient(t, address, testSigner(t))
	for i := 0; i < sessionsPerParticipant; i++ {
		session, err := squatter.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
	}
	session, err := dialSSHTestClient(t, address, testSigner(t)).NewSession()
	if err != nil {
		t.Fatalf("invited key could not open a session: %v", err)
	}
	defer session.Close()
	output, err := session.Output("join " + token)
	if err != nil || !strings.Contains(string(output), `"carol"`) {
		t.Fatalf("join failed: %v %q", err, output)
	}
}

// Catches a caller without a token rotating fresh keys to open unbounded
// sessions while an invite is pending: every unregistered admission counts
// against the IP's authentication window.
func TestUnregisteredAdmissionsCountPerIP(t *testing.T) {
	address, _, store := startSSHTestServer(t)
	now := time.Now()
	if err := store.CreateInvite("carol", "localhost", fmt.Sprintf("%x", sha256.Sum256([]byte("t"))), now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < authFailuresPerMinute; i++ {
		dialSSHTestClient(t, address, testSigner(t))
	}
	if client, err := ssh.Dial("tcp", address, sshTestConfig(testSigner(t))); err == nil {
		client.Close()
		t.Fatalf("unregistered key %d in one minute from one IP was admitted", authFailuresPerMinute+1)
	}
}
