package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gliderssh "github.com/gliderlabs/ssh"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sarfios20/mcp-agent-mailbox/mailbox"
	"golang.org/x/crypto/ssh"
)

type participantContextKey struct{}
type handshakeTimerContextKey struct{}
type publicKeyContextKey struct{}

type authWindow struct {
	started  time.Time
	failures int
}

// newSSHServer builds a key-authenticated server with only strict session channels.
func newSSHServer(store *mailbox.Store, dataDir string) (*gliderssh.Server, error) {
	signer, err := hostSigner(dataDir)
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	failures := make(map[string]authWindow)
	sessions := make(map[string]int)
	server := &gliderssh.Server{HostSigners: []gliderssh.Signer{signer}}
	server.ServerConfigCallback = func(ctx gliderssh.Context) *ssh.ServerConfig {
		return &ssh.ServerConfig{AuthLogCallback: func(metadata ssh.ConnMetadata, method string, err error) {
			if method == "publickey" && err == nil {
				ctx.Value(handshakeTimerContextKey{}).(*time.Timer).Stop()
				return
			}
			if err == nil || method == "none" {
				return
			}
			ip, _, splitErr := net.SplitHostPort(metadata.RemoteAddr().String())
			if splitErr != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			window := failures[ip]
			now := time.Now()
			if window.started.IsZero() || now.Sub(window.started) >= time.Minute {
				window = authWindow{started: now}
			}
			window.failures++
			failures[ip] = window
		}}
	}
	server.PublicKeyHandler = func(ctx gliderssh.Context, key gliderssh.PublicKey) bool {
		ip, _, err := net.SplitHostPort(ctx.RemoteAddr().String())
		if err != nil {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		for address, window := range failures {
			if now.Sub(window.started) >= time.Minute {
				delete(failures, address)
			}
		}
		window := failures[ip]
		if window.started.IsZero() {
			window.started = now
		}
		if window.failures >= authFailuresPerMinute {
			return false
		}
		publicKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		name, ok, err := store.ParticipantByKey(publicKey)
		if !validSSHKey(key) || err != nil || !ok {
			return false
		}
		ctx.SetValue(participantContextKey{}, name)
		ctx.SetValue(publicKeyContextKey{}, publicKey)
		return true
	}
	server.ChannelHandlers = map[string]gliderssh.ChannelHandler{"session": func(_ *gliderssh.Server, _ *ssh.ServerConn, incoming ssh.NewChannel, ctx gliderssh.Context) {
		name, _ := ctx.Value(participantContextKey{}).(string)
		publicKey, _ := ctx.Value(publicKeyContextKey{}).(string)
		mu.Lock()
		if sessions[name] >= sessionsPerParticipant {
			mu.Unlock()
			_ = incoming.Reject(ssh.ResourceShortage, "participant session limit reached")
			return
		}
		sessions[name]++
		mu.Unlock()
		defer func() {
			mu.Lock()
			sessions[name]--
			if sessions[name] == 0 {
				delete(sessions, name)
			}
			mu.Unlock()
		}()
		channel, requests, err := incoming.Accept()
		if err != nil {
			return
		}
		defer channel.Close()
		sessionCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		finished := make(chan error, 1)
		started := false
		execCommand := ""
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-finished:
				if err != nil {
					if execCommand == "wait" {
						refuseChannel(channel, "wait failed")
					} else {
						refuseChannel(channel, "MCP session failed")
					}
					return
				}
				exitChannel(channel, 0)
				return
			case request, ok := <-requests:
				if !ok {
					return
				}
				if request.Type != "exec" || started {
					_ = request.Reply(false, nil)
					refuseChannel(channel, "only an mcp or wait exec request is allowed")
					return
				}
				var command struct{ Command string }
				if ssh.Unmarshal(request.Payload, &command) != nil || (command.Command != "mcp" && command.Command != "wait") {
					_ = request.Reply(true, nil)
					refuseChannel(channel, "unknown command: only mcp and wait are supported")
					return
				}
				started = true
				execCommand = command.Command
				_ = request.Reply(true, nil)
				go func() {
					if command.Command == "wait" {
						unread, err := store.WaitUnread(sessionCtx, name)
						if err == nil {
							err = stillRegistered(store, name, publicKey)
						}
						if err == nil {
							err = json.NewEncoder(channel).Encode(struct {
								Unread int `json:"unread"`
							}{Unread: unread})
						}
						finished <- err
						return
					}
					finished <- newMCPServer(store, name, publicKey).Run(sessionCtx, &mcp.IOTransport{Reader: io.NopCloser(channel), Writer: nopCloser{channel}})
				}()
			}
		}
	}}
	// Global forwarding requests are rejected; a keepalive is the only accepted request.
	server.RequestHandlers = map[string]gliderssh.RequestHandler{
		"keepalive@openssh.com": func(gliderssh.Context, *gliderssh.Server, *ssh.Request) (bool, []byte) { return true, nil },
	}
	// Start one keepalive loop per authenticated connection, regardless of channel count.
	server.ConnCallback = func(ctx gliderssh.Context, conn net.Conn) net.Conn {
		// A deadline would not work: gliderlabs resets it on every read and write.
		ctx.SetValue(handshakeTimerContextKey{}, time.AfterFunc(handshakeTimeout, func() { _ = conn.Close() }))
		go func() {
			ticker := time.NewTicker(keepaliveInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if authenticated, ok := ctx.Value(gliderssh.ContextKeyConn).(*ssh.ServerConn); ok {
						// SendRequest waits for the reply with no limit of its own.
						unanswered := time.AfterFunc(keepaliveTimeout, func() { _ = conn.Close() })
						_, _, err := authenticated.SendRequest("keepalive@openssh.com", true, nil)
						unanswered.Stop()
						if err != nil {
							_ = conn.Close()
							return
						}
					}
				}
			}
		}()
		return conn
	}
	return server, nil
}

// stillRegistered fails once publicKey no longer belongs to name, so a connection
// opened before remove (or before the name got another key) cannot keep acting as name.
func stillRegistered(store *mailbox.Store, name, publicKey string) error {
	owner, ok, err := store.ParticipantByKey(publicKey)
	if err != nil {
		return err
	}
	if !ok || owner != name {
		return fmt.Errorf("the key of %s is no longer registered", name)
	}
	return nil
}

// validSSHKey limits authentication to the supported key types and RSA sizes.
func validSSHKey(key ssh.PublicKey) bool {
	public, ok := key.(ssh.CryptoPublicKey)
	if !ok {
		return false
	}
	switch key := public.CryptoPublicKey().(type) {
	case ed25519.PublicKey, *ecdsa.PublicKey:
		return true
	case *rsa.PublicKey:
		return key.N.BitLen() >= 2048
	default:
		return false
	}
}

// hostSigner loads the persistent OpenSSH host key, creating it only if absent.
func hostSigner(dataDir string) (ssh.Signer, error) {
	path := filepath.Join(dataDir, "host_ed25519_key")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		block, err := ssh.MarshalPrivateKey(key, "")
		if err != nil {
			return nil, err
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			data, err = os.ReadFile(path)
			if err != nil {
				return nil, err
			}
		} else {
			if err != nil {
				return nil, err
			}
			data = pem.EncodeToMemory(block)
			_, writeErr := file.Write(data)
			closeErr := file.Close()
			if writeErr != nil {
				return nil, writeErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
		}
	} else if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(data)
}

// refuseChannel gives every rejected session a one-line reason and failure status.
func refuseChannel(channel ssh.Channel, reason string) {
	_, _ = fmt.Fprintln(channel.Stderr(), reason)
	exitChannel(channel, 1)
}

// exitChannel sends the SSH exit status before its caller closes the channel.
func exitChannel(channel ssh.Channel, code uint32) {
	_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
}

// nopCloser lets MCP finish without closing the SSH channel before its exit status.
type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
