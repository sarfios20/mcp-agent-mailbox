package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"

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
func TestRejectsCommandsOtherThanMCP(t *testing.T) {
	address, keys, _ := startSSHTestServer(t)
	client := dialSSHTestClient(t, address, keys["alice"])
	for _, command := range []string{"ls", "wait", "mcp extra", ""} {
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
	server, err := newSSHServer(store, dataDir)
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
