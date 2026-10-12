package main

import "time"

const (
	// defaultDataDir holds the database, host key and listening port.
	defaultDataDir = "/var/lib/mcp-agent-mailbox"
	// defaultPort is the SSH port when serve or install has no port flag.
	defaultPort = 2222
	// authFailuresPerMinute limits failed key authentication attempts per IP.
	authFailuresPerMinute = 10
	// sessionsPerParticipant bounds concurrently open session channels per name.
	sessionsPerParticipant = 8
	// handshakeTimeout bounds SSH negotiation before authentication succeeds.
	handshakeTimeout = 10 * time.Second
	// keepaliveInterval sets the interval between SSH server keepalive requests.
	keepaliveInterval = 30 * time.Second
	// keepaliveTimeout closes a connection whose peer does not answer a keepalive in time.
	keepaliveTimeout = 15 * time.Second
	// maxTextBytes caps one message's text, so threads stay readable in one tool result.
	maxTextBytes = 64 << 10
	// maxSubjectBytes caps a thread subject.
	maxSubjectBytes = 256
	// maxThreadBytes caps the texts of one thread together. read_thread returns the
	// whole thread twice (structured and text) with JSON escaping, and SDK clients
	// reject responses over 16 MiB.
	maxThreadBytes = 1 << 20
)
