package admin

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/sarfios20/mcp-agent-mailbox/mailbox"
	"golang.org/x/crypto/ssh"
)

// BecomeDataOwner switches root to the data directory's owner before opening SQLite.
// Other callers must already own the directory; the identity change is permanent.
func BecomeDataOwner(dataDir string) error {
	info, err := os.Stat(dataDir)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot determine owner of data directory %q", dataDir)
	}
	uid, gid := int(stat.Uid), int(stat.Gid)
	if os.Geteuid() != 0 {
		if os.Geteuid() != uid {
			return fmt.Errorf("current user uid %d is not data directory owner uid %d", os.Geteuid(), uid)
		}
		return nil
	}
	if err := syscall.Setgroups([]int{gid}); err != nil {
		return err
	}
	if err := syscall.Setgid(gid); err != nil {
		return err
	}
	return syscall.Setuid(uid)
}

// Add registers a public key and returns client commands for the running SSH server.
// Repeating the same name and key leaves the registration unchanged.
func Add(name, pubkey, host, dataDir string) (any, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_.:-]+$`).MatchString(host) || strings.HasPrefix(host, "-") {
		return nil, fmt.Errorf("invalid SSH host %q", host)
	}
	key, err := readPublicKey(pubkey)
	if err != nil {
		return nil, err
	}
	if err := BecomeDataOwner(dataDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("server never started: run install or serve first")
		}
		return nil, err
	}
	var config struct {
		Port int `json:"port"`
	}
	data, err := os.ReadFile(filepath.Join(dataDir, "server.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("server never started: run install or serve first")
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	if config.Port < 1 || config.Port > 65535 {
		return nil, fmt.Errorf("invalid server port %d", config.Port)
	}
	privateKey, err := os.ReadFile(filepath.Join(dataDir, "host_ed25519_key"))
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	store, err := mailbox.Open(filepath.Join(dataDir, "mailbox.db"))
	if err != nil {
		return nil, err
	}
	defer store.Close()
	if err := store.AddParticipant(name, key); err != nil {
		return nil, err
	}
	args := []string{"-p", strconv.Itoa(config.Port), "-i", "~/.ssh/mcp-agent-mailbox", "-o", "IdentitiesOnly=yes", "mailbox@" + host, "mcp"}
	tomlArgs, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	command := fmt.Sprintf("ssh -p %d -i ~/.ssh/mcp-agent-mailbox -o IdentitiesOnly=yes mailbox@%s", config.Port, host)
	return map[string]any{"name": name, "client": map[string]string{
		"known_hosts": fmt.Sprintf("[%s]:%d %s", host, config.Port, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))),
		"claude_code": "claude mcp add --scope user mailbox -- " + command + " mcp",
		"codex_toml":  "[mcp_servers.mailbox]\ncommand = \"ssh\"\nargs = " + string(tomlArgs) + "\n",
		"wait":        command + " wait",
	}}, nil
}

// Remove revokes only the participant registration, preserving all mailbox history.
func Remove(name, dataDir string) (any, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	if err := BecomeDataOwner(dataDir); err != nil {
		return nil, err
	}
	store, err := mailbox.Open(filepath.Join(dataDir, "mailbox.db"))
	if err != nil {
		return nil, err
	}
	defer store.Close()
	removed, err := store.RemoveParticipant(name)
	if err != nil {
		return nil, err
	}
	return map[string]any{"removed": removed, "name": name}, nil
}

// validateName rejects participant names outside the mailbox name pattern.
func validateName(name string) error {
	if !regexp.MustCompile(ParticipantNamePattern).MatchString(name) {
		return fmt.Errorf("invalid participant name %q", name)
	}
	return nil
}

// readPublicKey parses exactly one bare public key and drops its optional comment.
func readPublicKey(path string) (string, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return "", err
	}
	key, _, options, rest, err := ssh.ParseAuthorizedKey(data)
	if err != nil || len(options) != 0 || len(strings.TrimSpace(string(rest))) != 0 {
		return "", errors.New("pubkey must contain exactly one bare OpenSSH public key")
	}
	cryptoKey, ok := key.(ssh.CryptoPublicKey)
	if !ok {
		return "", errors.New("unsupported public key type")
	}
	switch public := cryptoKey.CryptoPublicKey().(type) {
	case ed25519.PublicKey, *ecdsa.PublicKey:
	case *rsa.PublicKey:
		if public.N.BitLen() < 2048 {
			return "", errors.New("RSA public key must have at least 2048 bits")
		}
	default:
		return "", errors.New("public key must be ed25519, ecdsa or RSA with at least 2048 bits")
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), nil
}
