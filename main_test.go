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
	"github.com/sarfios20/mcp-agent-mailbox/buzon"
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

// Atrapa que el endpoint permita acceder sin token o con un token desconocido.
func TestAuthenticationRejectsMissingAndUnknownTokens(t *testing.T) {
	store := openHTTPTestStore(t)
	server := httptest.NewServer(newHandler(store, testTokens))
	t.Cleanup(server.Close)

	for _, test := range []struct {
		name  string
		token string
	}{
		{name: "missing"},
		{name: "unknown", token: "token-desconocido"},
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

// Atrapa que un campo sender inyectado suplante la identidad del bearer token.
func TestSendRejectsInjectedSender(t *testing.T) {
	store := openHTTPTestStore(t)
	server := httptest.NewServer(newHandler(store, testTokens))
	t.Cleanup(server.Close)
	ana := connectTestClient(t, server.URL+"/mcp", testTokens["ana"])

	result, err := ana.CallTool(context.Background(), &mcp.CallToolParams{Name: "send", Arguments: map[string]any{
		"to":      "bea",
		"subject": "estado",
		"text":    "mensaje suplantado",
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

// Atrapa que el transporte HTTP MCP impida que B vea en inbox el envío de A.
func TestSendAppearsInRecipientsInbox(t *testing.T) {
	store := openHTTPTestStore(t)
	server := httptest.NewServer(newHandler(store, testTokens))
	t.Cleanup(server.Close)
	ana := connectTestClient(t, server.URL+"/mcp", testTokens["ana"])
	bea := connectTestClient(t, server.URL+"/mcp", testTokens["bea"])

	sent := callTool[sendOutput](t, ana, "send", map[string]any{
		"to":      "bea",
		"subject": "entrega",
		"text":    "terminado",
	})
	inbox := callTool[inboxOutput](t, bea, "inbox", map[string]any{})
	if got := len(inbox.Threads); got != 1 {
		t.Fatalf("len(threads) = %d, want 1", got)
	}
	if got := inbox.Threads[0].ThreadID; got != sent.ThreadID {
		t.Fatalf("thread_id = %d, want %d", got, sent.ThreadID)
	}
	if got := inbox.Threads[0].With; got != "ana" {
		t.Fatalf("with = %q, want %q", got, "ana")
	}
}

// openHTTPTestStore abre un buzón aislado que se cierra automáticamente al
// terminar el test que lo usa.
func openHTTPTestStore(t *testing.T) *buzon.Store {
	t.Helper()
	store, err := buzon.Open(filepath.Join(t.TempDir(), "buzon.db"), []string{"ana", "bea"})
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

// connectTestClient conecta un cliente MCP autenticado y registra su cierre
// para que cada prueba use únicamente tráfico HTTP real contra httptest.
func connectTestClient(t *testing.T, endpoint, token string) *mcp.ClientSession {
	t.Helper()
	httpClient := &http.Client{Transport: bearerTransport{token: token}}
	client := mcp.NewClient(&mcp.Implementation{Name: "buzon-test", Version: "1.0.0"}, nil)
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

// callTool invoca una herramienta y decodifica su salida estructurada en el
// tipo esperado por el test.
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
