package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sarfios20/mcp-agent-mailbox/admin"
	"github.com/sarfios20/mcp-agent-mailbox/mailbox"
)

const serverVersion = "1.0.0"

type inboxInput struct{}

type participantsInput struct{}

type participantsOutput struct {
	Participants []string `json:"participants" jsonschema:"Every current participant name, sorted"`
}

type readThreadInput struct {
	ThreadID int64 `json:"thread_id" jsonschema:"ID of the thread to read"`
}

type replyInput struct {
	ThreadID int64  `json:"thread_id" jsonschema:"ID of the thread to reply to"`
	Text     string `json:"text" jsonschema:"Reply text"`
}

type addToThreadInput struct {
	ThreadID int64    `json:"thread_id" jsonschema:"ID of the thread to add participants to"`
	Names    []string `json:"names" jsonschema:"Participant names to add; wildcard is not supported"`
}

type addToThreadOutput struct {
	Participants []string `json:"participants" jsonschema:"All thread participant names after adding, sorted"`
}

type sendInput struct {
	To      []string `json:"to" jsonschema:"Recipient participant names or [\"*\"] for every other current participant"`
	Subject string   `json:"subject" jsonschema:"Subject of the new thread"`
	Text    string   `json:"text" jsonschema:"First message text"`
}

type inboxOutput struct {
	Threads []inboxEntryOutput `json:"threads" jsonschema:"Threads with unread messages"`
}

type inboxEntryOutput struct {
	ThreadID     int64    `json:"thread_id"`
	Subject      string   `json:"subject"`
	Participants []string `json:"participants"`
	Unread       int      `json:"unread"`
	LastAt       string   `json:"last_at"`
}

type readThreadOutput struct {
	Thread threadOutput `json:"thread" jsonschema:"Read thread with all its messages"`
}

type threadOutput struct {
	ID           int64           `json:"id"`
	Subject      string          `json:"subject"`
	Participants []string        `json:"participants"`
	Messages     []messageOutput `json:"messages"`
}

type messageOutput struct {
	ID        int64  `json:"id"`
	Sender    string `json:"sender"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
	Unread    bool   `json:"unread"`
}

type replyOutput struct {
	MessageID int64 `json:"message_id" jsonschema:"ID of the created message"`
}

type sendOutput struct {
	ThreadID int64 `json:"thread_id" jsonschema:"ID of the created thread"`
}

// newHandler builds the complete /mcp endpoint; each request must carry
// one of the configured tokens and tools use its associated name.
func newHandler(store *mailbox.Store, tokens map[string]string) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "mcp-agent-mailbox", Version: serverVersion}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "inbox",
		Description: "Lists threads with unread receipts for the authenticated participant, including all thread participants.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ inboxInput) (*mcp.CallToolResult, inboxOutput, error) {
		me, err := authenticatedName(req)
		if err != nil {
			return nil, inboxOutput{}, err
		}
		entries, err := store.Inbox(me)
		if err != nil {
			return nil, inboxOutput{}, err
		}
		out := inboxOutput{Threads: make([]inboxEntryOutput, 0, len(entries))}
		for _, entry := range entries {
			out.Threads = append(out.Threads, inboxEntryOutput{
				ThreadID:     entry.ThreadID,
				Subject:      entry.Subject,
				Participants: entry.Participants,
				Unread:       entry.Unread,
				LastAt:       entry.LastAt.Format(time.RFC3339Nano),
			})
		}
		return nil, out, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_thread",
		Description: "Returns a complete thread with its participants and each message’s unread state before this call, then marks only the authenticated participant’s receipts read.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input readThreadInput) (*mcp.CallToolResult, readThreadOutput, error) {
		me, err := authenticatedName(req)
		if err != nil {
			return nil, readThreadOutput{}, err
		}
		thread, err := store.ReadThread(me, input.ThreadID)
		if err != nil {
			return nil, readThreadOutput{}, err
		}
		return nil, readThreadOutput{Thread: translateThread(thread)}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "reply",
		Description: "Adds a reply to every other thread member; the sender is always the authenticated participant and must belong to the thread.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input replyInput) (*mcp.CallToolResult, replyOutput, error) {
		me, err := authenticatedName(req)
		if err != nil {
			return nil, replyOutput{}, err
		}
		messageID, err := store.Reply(me, input.ThreadID, input.Text)
		if err != nil {
			return nil, replyOutput{}, err
		}
		return nil, replyOutput{MessageID: messageID}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "send",
		Description: "Creates a thread with the recipient names in to, or [\"*\"] for every other current participant, and sends its first message; the sender is always the authenticated participant.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input sendInput) (*mcp.CallToolResult, sendOutput, error) {
		me, err := authenticatedName(req)
		if err != nil {
			return nil, sendOutput{}, err
		}
		threadID, err := store.Send(me, input.To, input.Subject, input.Text)
		if err != nil {
			return nil, sendOutput{}, err
		}
		return nil, sendOutput{ThreadID: threadID}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "add_to_thread",
		Description: "Adds named participants to a thread the authenticated participant belongs to and returns all thread participants sorted. New members see the whole history as read and receive unread receipts for future messages. Wildcard names and existing members are rejected.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input addToThreadInput) (*mcp.CallToolResult, addToThreadOutput, error) {
		me, err := authenticatedName(req)
		if err != nil {
			return nil, addToThreadOutput{}, err
		}
		participants, err := store.AddToThread(me, input.ThreadID, input.Names)
		if err != nil {
			return nil, addToThreadOutput{}, err
		}
		return nil, addToThreadOutput{Participants: participants}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "participants",
		Description: "Lists every currently configured participant name, sorted.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ participantsInput) (*mcp.CallToolResult, participantsOutput, error) {
		if _, err := authenticatedName(req); err != nil {
			return nil, participantsOutput{}, err
		}
		return nil, participantsOutput{Participants: store.Participants()}, nil
	})

	verifier := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		for name, expected := range tokens {
			if token == expected {
				return &auth.TokenInfo{UserID: name}, nil
			}
		}
		return nil, fmt.Errorf("unknown bearer token: %w", auth.ErrInvalidToken)
	}
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
	authenticated := auth.RequireBearerToken(verifier, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(mcpHandler)
	mux := http.NewServeMux()
	mux.Handle("/mcp", limitRequestBody(authenticated))
	return mux
}

func limitRequestBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxRequestBodyBytes {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		next.ServeHTTP(w, r)
	})
}

func authenticatedName(req *mcp.CallToolRequest) (string, error) {
	if req == nil || req.Extra == nil || req.Extra.TokenInfo == nil {
		return "", errors.New("MCP request without authenticated identity")
	}
	if req.Extra.TokenInfo.UserID == "" {
		return "", errors.New("MCP request with empty authenticated identity")
	}
	return req.Extra.TokenInfo.UserID, nil
}

func translateThread(thread mailbox.Thread) threadOutput {
	out := threadOutput{
		ID:           thread.ID,
		Subject:      thread.Subject,
		Participants: thread.Participants,
		Messages:     make([]messageOutput, 0, len(thread.Messages)),
	}
	for _, message := range thread.Messages {
		translated := messageOutput{
			ID:        message.ID,
			Sender:    message.Sender,
			Content:   message.Content,
			CreatedAt: message.CreatedAt.Format(time.RFC3339Nano),
			Unread:    message.Unread,
		}
		out.Messages = append(out.Messages, translated)
	}
	return out
}

// main dispatches non-interactive subcommands and emits one JSON result for administration.
func main() {
	os.Exit(run(os.Args[1:]))
}

// run validates subcommand arguments and returns the CLI exit status.
func run(args []string) int {
	usage := func() int {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]any{"error": "usage: mcp-agent-mailbox serve | install [--port N] | add <name> --pubkey <file|-> --host <ssh-host> | remove <name> | doctor | backup | uninstall"})
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	var result any
	var err error
	exitCode := 0
	switch args[0] {
	case "serve":
		if len(args) != 1 {
			return usage()
		}
		serve()
		return 0
	case "install":
		flags := flag.NewFlagSet("install", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		port := flags.Int("port", admin.DefaultPort, "loopback port")
		if flags.Parse(args[1:]) != nil || flags.NArg() != 0 || *port < 1 || *port > 65535 {
			return usage()
		}
		result, err = admin.Install(*port)
	case "add":
		if len(args) < 2 {
			return usage()
		}
		flags := flag.NewFlagSet("add", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		pubkey := flags.String("pubkey", "", "OpenSSH public key file or - for stdin")
		host := flags.String("host", "", "SSH host")
		name := args[1]
		if flags.Parse(args[2:]) != nil || flags.NArg() != 0 {
			return usage()
		}
		if *pubkey == "" || *host == "" {
			return usage()
		}
		result, err = admin.Add(name, *pubkey, *host)
	case "remove":
		if len(args) != 2 {
			return usage()
		}
		result, err = admin.Remove(args[1])
	case "doctor":
		if len(args) != 1 {
			return usage()
		}
		var diagnosis admin.DoctorResult
		diagnosis, err = admin.Doctor()
		result = diagnosis
		if !diagnosis.OK {
			exitCode = 1
		}
	case "backup":
		if len(args) != 1 {
			return usage()
		}
		result, err = admin.Backup()
	case "uninstall":
		if len(args) != 1 {
			return usage()
		}
		result, err = admin.Uninstall()
	default:
		return usage()
	}
	if err != nil {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]any{"error": err.Error()})
		return 1
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
	return exitCode
}

// serve opens the mailbox and serves the authenticated MCP endpoint.
func serve() {
	config, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	participants := make([]string, 0, len(config.Tokens))
	for name := range config.Tokens {
		participants = append(participants, name)
	}
	sort.Strings(participants)

	store, err := mailbox.Open(config.DBPath, participants)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Printf("close mailbox: %v", err)
		}
	}()

	server := &http.Server{
		Addr:              config.Addr,
		Handler:           newHandler(store, config.Tokens),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
