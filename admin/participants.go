package admin

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

// Add provisions a restricted tunnel account and returns its token and client configuration.
// Repeating an existing name and key returns the same token without changing host state.
func Add(name, pubkey, host string) (any, error) {
	if err := requireRoot(); err != nil {
		return nil, err
	}
	var config struct {
		Port int `json:"port"`
	}
	data, err := os.ReadFile(filepath.Join(ConfigDir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("not installed: run install first")
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	if config.Port < 1 || config.Port > 65535 {
		return nil, fmt.Errorf("invalid installed port %d", config.Port)
	}
	if err := validateName(name); err != nil {
		return nil, err
	}
	if strings.HasPrefix(host, "-") || strings.ContainsAny(host, " \t\r\n\x00") || host == "" {
		return nil, fmt.Errorf("invalid SSH host %q", host)
	}
	key, err := readPublicKey(pubkey)
	if err != nil {
		return nil, err
	}
	tokens, err := readTokens()
	if err != nil {
		return nil, err
	}
	user := "mailbox-" + name
	keyPath := filepath.Join(KeysDir, user)
	keyLine := fmt.Sprintf("restrict,port-forwarding,permitopen=\"127.0.0.1:%d\" %s\n", config.Port, key)
	if token, exists := tokens[name]; exists {
		stored, err := os.ReadFile(keyPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		_, storedKey, _ := strings.Cut(strings.TrimSuffix(string(stored), "\n"), " ")
		if storedKey != key {
			return nil, fmt.Errorf("participant %s exists with a different key; remove it first", name)
		}
		return clientResult(name, token, host, config.Port)
	}
	random := make([]byte, TokenBytes)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(random)
	result, err := clientResult(name, token, host, config.Port)
	if err != nil {
		return nil, err
	}
	if _, exists, err := lookup("passwd", user); err != nil {
		return nil, err
	} else if !exists {
		if _, err := command("useradd", "--system", "--no-create-home", "--home-dir", "/nonexistent", "--shell", "/usr/sbin/nologin", "--gid", TunnelGroup, user); err != nil {
			return nil, err
		}
	}
	if err := writeFile(keyPath, []byte(keyLine), 0644); err != nil {
		return nil, err
	}
	tokens[name] = token
	if err := writeTokens(tokens); err != nil {
		return nil, err
	}
	if _, err := command("systemctl", "restart", ServiceName); err != nil {
		return nil, err
	}
	return result, nil
}

// Remove revokes a token and key and terminates the participant's existing SSH tunnels.
// Removing an absent participant succeeds and reports removed=false.
func Remove(name string) (any, error) {
	if err := requireRoot(); err != nil {
		return nil, err
	}
	if err := validateName(name); err != nil {
		return nil, err
	}
	tokens, err := readTokens()
	if err != nil {
		return nil, err
	}
	_, removed := tokens[name]
	user := "mailbox-" + name
	if _, exists, err := lookup("passwd", user); err != nil {
		return nil, err
	} else if exists {
		if err := deleteTunnelUser(user); err != nil {
			return nil, err
		}
		removed = true
	}
	keyPath := filepath.Join(KeysDir, user)
	if err := os.Remove(keyPath); err == nil {
		removed = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if removed {
		delete(tokens, name)
		if err := writeTokens(tokens); err != nil {
			return nil, err
		}
		if _, err := command("systemctl", "restart", ServiceName); err != nil {
			return nil, err
		}
	}
	return map[string]any{"removed": removed, "name": name}, nil
}

// validateName rejects participant names unsafe for Unix accounts and key paths.
func validateName(name string) error {
	if !regexp.MustCompile(ParticipantNamePattern).MatchString(name) {
		return fmt.Errorf("invalid participant name %q", name)
	}
	return nil
}

// readPublicKey accepts exactly one bare OpenSSH public key line, verified by ssh-keygen.
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
	key := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if strings.ContainsAny(key, "\r\n\x00") {
		return "", errors.New("pubkey must contain exactly one OpenSSH public key line")
	}
	key = strings.TrimSpace(key)
	fields := strings.Fields(key)
	if len(fields) < 2 || !(strings.HasPrefix(fields[0], "ssh-") || strings.HasPrefix(fields[0], "ecdsa-") || strings.HasPrefix(fields[0], "sk-")) {
		return "", errors.New("pubkey must contain exactly one OpenSSH public key line")
	}
	file, err := os.CreateTemp("", ".mailbox-pubkey-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	if _, err := file.WriteString(key + "\n"); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if _, err := command("ssh-keygen", "-l", "-f", file.Name()); err != nil {
		return "", err
	}
	return key, nil
}

// readTokens reads the participant map, treating a missing file as an empty installation.
func readTokens() (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(ConfigDir, "tokens.json"))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var tokens map[string]string
	if err := json.Unmarshal(data, &tokens); err != nil {
		return nil, err
	}
	if tokens == nil {
		return nil, errors.New("tokens.json must be a JSON object")
	}
	return tokens, nil
}

// writeTokens atomically replaces tokens.json with root-only permissions.
func writeTokens(tokens map[string]string) error {
	data, err := json.Marshal(tokens)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(ConfigDir, "tokens.json"), append(data, '\n'), 0600)
}

// deleteTunnelUser kills active sessions before deleting an existing tunnel account.
func deleteTunnelUser(user string) error {
	if _, err := command("pkill", "-KILL", "-u", user); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return err
		}
	}
	_, err := command("userdel", user)
	return err
}

// clientResult renders ready-to-paste client strings from the embedded templates.
func clientResult(name, token, host string, port int) (any, error) {
	client := make(map[string]string)
	settings := map[string]any{"Host": host, "User": "mailbox-" + name, "Port": port, "Token": token}
	for _, name := range []string{"ssh_config", "tunnel_systemd", "tunnel_launchd", "tunnel_windows", "claude_code", "codex_toml"} {
		t, err := template.ParseFS(units, "units/client/"+name)
		if err != nil {
			return nil, err
		}
		var rendered bytes.Buffer
		if err := t.Execute(&rendered, settings); err != nil {
			return nil, err
		}
		client[name] = rendered.String()
	}
	return map[string]any{"name": name, "token": token, "ssh_user": settings["User"], "client": client}, nil
}
