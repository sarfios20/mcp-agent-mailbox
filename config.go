package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// HTTP server limits. They do not depend on the environment: change them here.
const (
	// maxRequestBodyBytes is the maximum MCP request size; larger requests receive 413.
	maxRequestBodyBytes int64 = 1 << 20
	// readHeaderTimeout limits how long a client can take to send headers.
	readHeaderTimeout = 5 * time.Second
	// readTimeout limits reading the entire request, including its body.
	readTimeout = 30 * time.Second
	// writeTimeout limits how long writing the response can take.
	writeTimeout = 60 * time.Second
	// idleTimeout closes keep-alive connections that have been idle for this long.
	idleTimeout = 120 * time.Second
)

// Config holds all server startup settings.
type Config struct {
	// DBPath is the SQLite file path configured with MAILBOX_DB.
	DBPath string
	// TokensFile is the JSON file path configured with MAILBOX_TOKENS_FILE.
	TokensFile string
	// Addr is the listening host:port configured with MAILBOX_ADDR.
	Addr string
	// Tokens maps each participant to their bearer token.
	Tokens map[string]string
}

// loadConfig reads the environment and tokens file, and only returns a
// configuration ready to open the mailbox and start the HTTP server.
func loadConfig() (Config, error) {
	dbPath, err := requiredEnv("MAILBOX_DB")
	if err != nil {
		return Config{}, err
	}
	tokensFile, err := requiredEnv("MAILBOX_TOKENS_FILE")
	if err != nil {
		return Config{}, err
	}
	addr, err := requiredEnv("MAILBOX_ADDR")
	if err != nil {
		return Config{}, err
	}
	if err := validateAddr(addr); err != nil {
		return Config{}, fmt.Errorf("MAILBOX_ADDR %q: %w", addr, err)
	}

	data, err := os.ReadFile(tokensFile)
	if err != nil {
		return Config{}, fmt.Errorf("MAILBOX_TOKENS_FILE %q: %w", tokensFile, err)
	}
	var tokens map[string]string
	if err := json.Unmarshal(data, &tokens); err != nil {
		return Config{}, fmt.Errorf("MAILBOX_TOKENS_FILE %q: invalid JSON: %w", tokensFile, err)
	}
	if tokens == nil {
		return Config{}, fmt.Errorf("MAILBOX_TOKENS_FILE %q: must be a JSON object", tokensFile)
	}
	seenTokens := make(map[string]string, len(tokens))
	for name, token := range tokens {
		if strings.TrimSpace(name) == "" {
			return Config{}, fmt.Errorf("MAILBOX_TOKENS_FILE %q: empty participant name", tokensFile)
		}
		if strings.TrimSpace(token) == "" {
			return Config{}, fmt.Errorf("MAILBOX_TOKENS_FILE %q: empty token for %q", tokensFile, name)
		}
		if previous, exists := seenTokens[token]; exists {
			return Config{}, fmt.Errorf("MAILBOX_TOKENS_FILE %q: duplicate token for %q and %q", tokensFile, previous, name)
		}
		seenTokens[token] = name
	}

	return Config{DBPath: dbPath, TokensFile: tokensFile, Addr: addr, Tokens: tokens}, nil
}

func requiredEnv(name string) (string, error) {
	value := os.Getenv(name)
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is missing or empty", name)
	}
	return value, nil
}

func validateAddr(addr string) error {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must have host:port format: %w", err)
	}
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("host %q must be loopback", host)
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("port %q is invalid", portText)
	}
	return nil
}
