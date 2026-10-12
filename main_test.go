package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sarfios20/mcp-agent-mailbox/mailbox"
)

func TestRequestBodyLimit(t *testing.T) {
	store := openHTTPTestStore(t)
	server := httptest.NewServer(newHandler(store, testTokens))
	t.Cleanup(server.Close)

	request, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/mcp",
		strings.NewReader(strings.Repeat("x", int(maxRequestBodyBytes)+1)),
	)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+testTokens["ana"])
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusRequestEntityTooLarge)
	}
}

var testTokens = map[string]string{
	"ana": "token-ana",
	"bea": "token-bea",
}

// Catches the endpoint allowing access without a token or with an unknown token.
func TestAuthenticationRejectsMissingAndUnknownTokens(t *testing.T) {
	store := openHTTPTestStore(t)
	server := httptest.NewServer(newHandler(store, testTokens))
	t.Cleanup(server.Close)

	for _, test := range []struct {
		name  string
		token string
	}{
		{name: "missing"},
		{name: "unknown", token: "token-unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, server.URL+"/mcp", nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			if test.token != "" {
				req.Header.Set("Authorization", "Bearer "+test.token)
			}
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
			}
		})
	}
}

// Catches an injected sender field impersonating the bearer token identity.
func TestSendRejectsInjectedSender(t *testing.T) {
	store := openHTTPTestStore(t)
	server := httptest.NewServer(newHandler(store, testTokens))
	t.Cleanup(server.Close)
	ana := connectTestClient(t, server.URL+"/mcp", testTokens["ana"])

	result, err := ana.CallTool(context.Background(), &mcp.CallToolParams{Name: "send", Arguments: map[string]any{
		"to":      []string{"bea"},
		"subject": "status",
		"text":    "spoofed message",
		"sender":  "bea",
	}})
	if err != nil {
		t.Fatalf("CallTool(send): %v", err)
	}
	if !result.IsError {
		t.Fatal("send with an injected sender field was accepted")
	}
	inbox, err := store.Inbox("bea")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(inbox) != 0 {
		t.Fatalf("rejected send left %d threads in the recipient inbox", len(inbox))
	}
}

// Catches the MCP HTTP transport preventing B from seeing A's message in inbox.
func TestSendAppearsInRecipientsInbox(t *testing.T) {
	store := openHTTPTestStore(t)
	server := httptest.NewServer(newHandler(store, testTokens))
	t.Cleanup(server.Close)
	ana := connectTestClient(t, server.URL+"/mcp", testTokens["ana"])
	bea := connectTestClient(t, server.URL+"/mcp", testTokens["bea"])

	sent := callTool[sendOutput](t, ana, "send", map[string]any{
		"to":      []string{"bea"},
		"subject": "delivery",
		"text":    "done",
	})
	inbox := callTool[inboxOutput](t, bea, "inbox", map[string]any{})
	if got := len(inbox.Threads); got != 1 {
		t.Fatalf("len(threads) = %d, want 1", got)
	}
	if got := inbox.Threads[0].ThreadID; got != sent.ThreadID {
		t.Fatalf("thread_id = %d, want %d", got, sent.ThreadID)
	}
	if got := inbox.Threads[0].Participants; len(got) != 2 || got[0] != "ana" || got[1] != "bea" {
		t.Fatalf("participants = %q, want [ana bea]", got)
	}
}

// openHTTPTestStore opens an isolated mailbox that closes automatically when
// the test using it finishes.
func openHTTPTestStore(t *testing.T) *mailbox.Store {
	t.Helper()
	store, err := mailbox.Open(filepath.Join(t.TempDir(), "mailbox.db"), []string{"ana", "bea"})
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

// connectTestClient connects an authenticated MCP client and registers its cleanup
// so each test uses only real HTTP traffic against httptest.
func connectTestClient(t *testing.T, endpoint, token string) *mcp.ClientSession {
	t.Helper()
	httpClient := &http.Client{Transport: bearerTransport{token: token}}
	client := mcp.NewClient(&mcp.Implementation{Name: "mailbox-test", Version: "1.0.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           httpClient,
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("Close session: %v", err)
		}
	})
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

type bearerTransport struct {
	token string
}

func (transport bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+transport.token)
	return http.DefaultTransport.RoundTrip(clone)
}
