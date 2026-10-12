package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sarfios20/mcp-agent-mailbox/buzon"
)

const serverVersion = "1.0.0"

type inboxInput struct{}

type readThreadInput struct {
	ThreadID int64 `json:"thread_id" jsonschema:"Identificador del hilo que se quiere leer"`
}

type replyInput struct {
	ThreadID int64  `json:"thread_id" jsonschema:"Identificador del hilo al que se responde"`
	Text     string `json:"text" jsonschema:"Texto de la respuesta"`
}

type sendInput struct {
	To      string `json:"to" jsonschema:"Nombre del participante destinatario"`
	Subject string `json:"subject" jsonschema:"Asunto del nuevo hilo"`
	Text    string `json:"text" jsonschema:"Texto del primer mensaje"`
}

type inboxOutput struct {
	Threads []inboxEntryOutput `json:"threads" jsonschema:"Hilos con mensajes pendientes de leer"`
}

type inboxEntryOutput struct {
	ThreadID int64  `json:"thread_id"`
	Subject  string `json:"subject"`
	With     string `json:"with"`
	Unread   int    `json:"unread"`
	LastAt   string `json:"last_at"`
}

type readThreadOutput struct {
	Thread threadOutput `json:"thread" jsonschema:"Hilo leído con todos sus mensajes"`
}

type threadOutput struct {
	ID       int64           `json:"id"`
	Subject  string          `json:"subject"`
	With     string          `json:"with"`
	Messages []messageOutput `json:"messages"`
}

type messageOutput struct {
	ID        int64   `json:"id"`
	Sender    string  `json:"sender"`
	Recipient string  `json:"recipient"`
	Content   string  `json:"content"`
	CreatedAt string  `json:"created_at"`
	ReadAt    *string `json:"read_at"`
}

type replyOutput struct {
	MessageID int64 `json:"message_id" jsonschema:"Identificador del mensaje creado"`
}

type sendOutput struct {
	ThreadID int64 `json:"thread_id" jsonschema:"Identificador del hilo creado"`
}

// newHandler construye el endpoint /mcp completo; cada petición debe llevar
// uno de los tokens configurados y las herramientas usan su nombre asociado.
func newHandler(store *buzon.Store, tokens map[string]string) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "buzon", Version: serverVersion}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "inbox",
		Description: "Lista los hilos que contienen mensajes sin leer dirigidos al participante autenticado.",
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
				ThreadID: entry.ThreadID,
				Subject:  entry.Subject,
				With:     entry.With,
				Unread:   entry.Unread,
				LastAt:   entry.LastAt.Format(time.RFC3339Nano),
			})
		}
		return nil, out, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_thread",
		Description: "Devuelve un hilo completo y marca como leídos solo los mensajes recibidos por el participante autenticado.",
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
		Description: "Añade una respuesta al hilo dirigida al otro participante; el remitente es siempre el participante autenticado.",
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
		Description: "Crea un hilo y envía su primer mensaje; el remitente es siempre el participante autenticado.",
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

	verifier := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		for name, expected := range tokens {
			if token == expected {
				return &auth.TokenInfo{UserID: name}, nil
			}
		}
		return nil, fmt.Errorf("bearer token desconocido: %w", auth.ErrInvalidToken)
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
		return "", errors.New("petición MCP sin identidad autenticada")
	}
	if req.Extra.TokenInfo.UserID == "" {
		return "", errors.New("petición MCP con identidad autenticada vacía")
	}
	return req.Extra.TokenInfo.UserID, nil
}

func translateThread(thread buzon.Thread) threadOutput {
	out := threadOutput{
		ID:       thread.ID,
		Subject:  thread.Subject,
		With:     thread.With,
		Messages: make([]messageOutput, 0, len(thread.Messages)),
	}
	for _, message := range thread.Messages {
		translated := messageOutput{
			ID:        message.ID,
			Sender:    message.Sender,
			Recipient: message.Recipient,
			Content:   message.Content,
			CreatedAt: message.CreatedAt.Format(time.RFC3339Nano),
		}
		if message.ReadAt != nil {
			readAt := message.ReadAt.Format(time.RFC3339Nano)
			translated.ReadAt = &readAt
		}
		out.Messages = append(out.Messages, translated)
	}
	return out
}

func main() {
	config, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	participants := make([]string, 0, len(config.Tokens))
	for name := range config.Tokens {
		participants = append(participants, name)
	}
	sort.Strings(participants)

	store, err := buzon.Open(config.DBPath, participants)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Printf("cerrar buzón: %v", err)
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
