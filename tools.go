package main

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// newMCPServer binds all tools to the participant authenticated by SSH with publicKey.
// Every tool call first checks that publicKey still belongs to me.
func newMCPServer(store *mailbox.Store, me, publicKey string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "mcp-agent-mailbox", Version: serverVersion}, nil)
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/call" {
				if err := stillRegistered(store, me, publicKey); err != nil {
					return nil, err
				}
			}
			return next(ctx, method, req)
		}
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "inbox",
		Description: "Lists threads with unread receipts for the authenticated participant, including all thread participants.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ inboxInput) (*mcp.CallToolResult, inboxOutput, error) {
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
		if err := checkSize("text", input.Text, maxTextBytes); err != nil {
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
		if err := checkSize("subject", input.Subject, maxSubjectBytes); err != nil {
			return nil, sendOutput{}, err
		}
		if err := checkSize("text", input.Text, maxTextBytes); err != nil {
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
		names, err := store.Participants()
		return nil, participantsOutput{Participants: names}, err
	})

	return server
}

// checkSize rejects a field longer than limit bytes, naming the field and both sizes.
func checkSize(field, value string, limit int) error {
	if len(value) > limit {
		return fmt.Errorf("%s is %d bytes, the limit is %d", field, len(value), limit)
	}
	return nil
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
