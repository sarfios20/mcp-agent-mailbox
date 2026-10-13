package main

import "time"

const (
	// defaultDataDir holds the database, host key and listening port.
	defaultDataDir = "/var/lib/mcp-agent-mailbox"
	// defaultPort is the SSH port when serve or install has no port flag.
	defaultPort = 2222
	// authFailuresPerMinute limits failed key authentication attempts per IP,
	// counting every admission of a key that is not registered yet.
	authFailuresPerMinute = 10
	// sessionsPerParticipant bounds concurrently open session channels per name,
	// and per key for keys that are not registered yet.
	sessionsPerParticipant = 8
	// handshakeTimeout bounds SSH negotiation before authentication succeeds.
	handshakeTimeout = 10 * time.Second
	// joinTimeout closes unregistered connections this long after authentication, even after joining.
	joinTimeout = 30 * time.Second
	// keepaliveInterval sets the interval between SSH server keepalive requests.
	keepaliveInterval = 30 * time.Second
	// keepaliveTimeout closes a connection whose peer does not answer a keepalive in time.
	keepaliveTimeout = 15 * time.Second
	// maxTextBytes caps one message's text.
	maxTextBytes = 64 << 10
	// maxSubjectBytes caps a thread subject.
	maxSubjectBytes = 256
	// maxPageMessages caps the number of messages in one read_thread page.
	maxPageMessages = 200
	// maxPageBytes caps the text bytes of one page. SDK clients reject results
	// over 16 MiB, and a page is encoded twice with JSON escaping.
	maxPageBytes = 1 << 20
)
