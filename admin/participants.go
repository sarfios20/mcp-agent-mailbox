package admin

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
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
	"time"

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
	if err := validateHost(host); err != nil {
		return nil, err
	}
	key, err := readPublicKey(pubkey)
	if err != nil {
		return nil, err
	}
	port, hostKey, err := clientServer(dataDir)
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
	return map[string]any{"name": name, "client": ClientCommands(host, port, hostKey)}, nil
}

// ClientCommands formats the four client setup strings for a server and its host key.
func ClientCommands(host string, port int, hostKey ssh.PublicKey) map[string]string {
	args := []string{"-p", strconv.Itoa(port), "-i", "~/.ssh/mcp-agent-mailbox", "-o", "IdentitiesOnly=yes", "mailbox@" + host, "mcp"}
	tomlArgs, _ := json.Marshal(args)
	command := fmt.Sprintf("ssh -p %d -i ~/.ssh/mcp-agent-mailbox -o IdentitiesOnly=yes mailbox@%s", port, host)
	return map[string]string{
		"known_hosts": fmt.Sprintf("[%s]:%d %s", host, port, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey)))),
		"claude_code": "claude mcp add --scope user mailbox -- " + command + " mcp",
		"codex_toml":  "[mcp_servers.mailbox]\ncommand = \"ssh\"\nargs = " + string(tomlArgs) + "\n",
		"wait":        command + " wait",
	}
}

// Invite creates a one-time token and returns the SSH command to redeem it.
func Invite(name, host, dataDir string) (any, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	if err := validateHost(host); err != nil {
		return nil, err
	}
	port, hostKey, err := clientServer(dataDir)
	if err != nil {
		return nil, err
	}
	tokenBytes := make([]byte, InviteTokenBytes)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	hash := sha256.Sum256([]byte(token))
	now := time.Now().UTC()
	expiresAt := now.Add(InviteTTL)
	store, err := mailbox.Open(filepath.Join(dataDir, "mailbox.db"))
	if err != nil {
		return nil, err
	}
	defer store.Close()
	if err := store.CreateInvite(name, host, fmt.Sprintf("%x", hash), now, expiresAt); err != nil {
		return nil, err
	}
	return map[string]any{"name": name, "expires_at": expiresAt.Format(time.RFC3339), "client": map[string]string{
		"known_hosts": ClientCommands(host, port, hostKey)["known_hosts"],
		"join":        fmt.Sprintf("ssh -p %d -i ~/.ssh/mcp-agent-mailbox -o IdentitiesOnly=yes mailbox@%s join %s", port, host, token),
	}}, nil
}

// clientServer drops to the data owner and reads the running server's port and host key.
func clientServer(dataDir string) (int, ssh.PublicKey, error) {
	if err := BecomeDataOwner(dataDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil, errors.New("server never started: run install or serve first")
		}
		return 0, nil, err
	}
	var config struct {
		Port int `json:"port"`
	}
	data, err := os.ReadFile(filepath.Join(dataDir, "server.json"))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil, errors.New("server never started: run install or serve first")
	}
	if err != nil {
		return 0, nil, err
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return 0, nil, err
	}
	if config.Port < 1 || config.Port > 65535 {
		return 0, nil, fmt.Errorf("invalid server port %d", config.Port)
	}
	privateKey, err := os.ReadFile(filepath.Join(dataDir, "host_ed25519_key"))
	if err != nil {
		return 0, nil, err
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return 0, nil, err
	}
	return config.Port, signer.PublicKey(), nil
}

// validateHost restricts hosts to values that can be embedded in SSH commands.
func validateHost(host string) error {
	if !regexp.MustCompile(`^[a-zA-Z0-9_.:-]+$`).MatchString(host) || strings.HasPrefix(host, "-") {
		return fmt.Errorf("invalid SSH host %q", host)
	}
	return nil
}

// Remove revokes the participant registration and any invite, preserving mailbox history.
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
