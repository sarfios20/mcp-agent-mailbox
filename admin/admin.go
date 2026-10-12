package admin

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"text/template"
)

//go:embed units/*
var units embed.FS

// Install provisions the root-owned configuration, systemd units, and validated SSH drop-in.
// Existing tokens and data survive repeated calls; port changes restart an active server.
func Install(port int) (any, error) {
	if err := requireRoot(); err != nil {
		return nil, err
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid port %d", port)
	}
	if err := supportedOS(); err != nil {
		return nil, err
	}
	for _, tool := range []string{"systemctl", "sshd", "ss", "useradd", "groupadd", "getent"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, fmt.Errorf("required command %s: %w", tool, err)
		}
	}
	if err := checkSSHInclude(); err != nil {
		return nil, err
	}
	if err := checkPort(port); err != nil {
		return nil, err
	}
	active, err := command("systemctl", "show", "--property=ActiveState", "--value", ServiceName)
	if err != nil {
		return nil, err
	}
	if err := copyExecutable(); err != nil {
		return nil, err
	}
	if _, exists, err := lookup("passwd", SystemUser); err != nil {
		return nil, err
	} else if !exists {
		if _, err := command("useradd", "--system", "--user-group", "--no-create-home", "--shell", "/usr/sbin/nologin", SystemUser); err != nil {
			return nil, err
		}
	}
	if _, exists, err := lookup("group", TunnelGroup); err != nil {
		return nil, err
	} else if !exists {
		if _, err := command("groupadd", "--system", TunnelGroup); err != nil {
			return nil, err
		}
	}
	for _, dir := range []string{ConfigDir, BackupDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		if err := os.Chown(dir, 0, 0); err != nil {
			return nil, err
		}
		if err := os.Chmod(dir, 0700); err != nil {
			return nil, err
		}
	}
	tokens := filepath.Join(ConfigDir, "tokens.json")
	file, err := os.OpenFile(tokens, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		_, writeErr := file.WriteString("{}\n")
		closeErr := file.Close()
		if writeErr != nil {
			return nil, writeErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err := writeFile(filepath.Join(ConfigDir, "config.json"), []byte(fmt.Sprintf("{\"port\":%d}\n", port)), 0600); err != nil {
		return nil, err
	}
	for _, name := range []string{ServiceName, BackupServiceName, BackupTimerName} {
		if err := writeTemplate(name, filepath.Join(UnitDir, name), port); err != nil {
			return nil, err
		}
	}
	if err := writeTemplate(filepath.Base(DropInPath), DropInPath, port); err != nil {
		return nil, err
	}
	if _, err := command("sshd", "-t"); err != nil {
		if removeErr := os.Remove(DropInPath); removeErr != nil {
			return nil, fmt.Errorf("%w; remove invalid drop-in: %v", err, removeErr)
		}
		return nil, err
	}
	if err := reloadSSH(); err != nil {
		return nil, err
	}
	if _, err := command("systemctl", "daemon-reload"); err != nil {
		return nil, err
	}
	if _, err := command("systemctl", "enable", "--now", ServiceName, BackupTimerName); err != nil {
		return nil, err
	}
	if strings.TrimSpace(active) == "active" {
		if _, err := command("systemctl", "restart", ServiceName); err != nil {
			return nil, err
		}
	}
	return map[string]any{"installed": true, "port": port, "binary": BinaryPath, "config_dir": ConfigDir, "data_dir": DataDir, "backup_dir": BackupDir}, nil
}

// Uninstall removes managed units and tunnel accounts while preserving the server user and all persistent files.
func Uninstall() (any, error) {
	if err := requireRoot(); err != nil {
		return nil, err
	}
	for _, name := range []string{ServiceName, BackupTimerName, BackupServiceName} {
		state, err := command("systemctl", "show", "--property=LoadState", "--value", name)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(state) != "not-found" {
			if _, err := command("systemctl", "disable", "--now", name); err != nil {
				return nil, err
			}
		}
	}
	for _, name := range []string{ServiceName, BackupServiceName, BackupTimerName} {
		if err := removeFile(filepath.Join(UnitDir, name)); err != nil {
			return nil, err
		}
	}
	if _, err := command("systemctl", "daemon-reload"); err != nil {
		return nil, err
	}
	group, exists, err := lookup("group", TunnelGroup)
	if err != nil {
		return nil, err
	}
	if exists {
		fields := strings.Split(strings.TrimSpace(group), ":")
		if len(fields) != 4 {
			return nil, fmt.Errorf("invalid getent group output: %q", group)
		}
		members := make(map[string]bool)
		for _, name := range strings.Split(fields[3], ",") {
			members[name] = true
		}
		passwd, err := command("getent", "passwd")
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(strings.TrimSpace(passwd), "\n") {
			account := strings.Split(line, ":")
			if len(account) != 7 {
				return nil, fmt.Errorf("invalid getent passwd output: %q", line)
			}
			if account[0] != SystemUser && (account[3] == fields[2] || members[account[0]]) {
				if _, err := command("userdel", "--force", account[0]); err != nil {
					return nil, err
				}
			}
		}
		if _, err := command("groupdel", TunnelGroup); err != nil {
			return nil, err
		}
	}
	if err := removeFile(DropInPath); err != nil {
		return nil, err
	}
	if _, err := command("sshd", "-t"); err != nil {
		return nil, err
	}
	if err := reloadSSH(); err != nil {
		return nil, err
	}
	return map[string]any{"uninstalled": true, "kept": []string{ConfigDir, DataDir, BackupDir}}, nil
}

// command captures tool output without leaking it onto the CLI's JSON streams.
func command(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("command %s %s: %w; stderr: %s; stdout: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()), strings.TrimSpace(stdout.String()))
	}
	return stdout.String(), nil
}

// lookup distinguishes absent NSS records from a failed system command.
func lookup(database, name string) (string, bool, error) {
	out, err := command("getent", database, name)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 2 {
			return "", false, nil
		}
		return "", false, err
	}
	return out, true, nil
}

// requireRoot rejects administrative operations before they change the host.
func requireRoot() error {
	if os.Geteuid() != 0 {
		return errors.New("this subcommand requires root")
	}
	return nil
}

// supportedOS accepts only the distributions, minimum versions, and architectures in the installation contract.
func supportedOS() error {
	data, err := os.ReadFile(OSReleasePath)
	if err != nil {
		return fmt.Errorf("detect OS from %s: %w", OSReleasePath, err)
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = strings.Trim(value, "\"'")
		}
	}
	id, version := values["ID"], values["VERSION_ID"]
	parts := strings.Split(version, ".")
	major, parseErr := strconv.Atoi(parts[0])
	minor := 0
	if len(parts) > 1 {
		minor, err = strconv.Atoi(parts[1])
	}
	supported := parseErr == nil && err == nil && ((id == "debian" && major >= 12) || (id == "ubuntu" && (major > 22 || major == 22 && minor >= 4)))
	if !supported || runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return fmt.Errorf("unsupported OS ID=%q VERSION_ID=%q (%s/%s)", id, version, runtime.GOOS, runtime.GOARCH)
	}
	return nil
}

// checkSSHInclude requires a global Include whose glob covers the managed drop-in.
func checkSSHInclude() error {
	data, err := os.ReadFile(SSHConfigPath)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line, _, _ = strings.Cut(line, "#")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if strings.EqualFold(fields[0], "Match") {
			break
		}
		if !strings.EqualFold(fields[0], "Include") {
			continue
		}
		for _, pattern := range fields[1:] {
			pattern = strings.Trim(pattern, "\"'")
			if !filepath.IsAbs(pattern) {
				pattern = filepath.Join("/etc/ssh", pattern)
			}
			if match, _ := filepath.Match(pattern, DropInPath); match {
				return nil
			}
		}
	}
	return fmt.Errorf("%s must have a global Include covering %s", SSHConfigPath, filepath.Dir(DropInPath))
}

// checkPort permits existing listeners only when every reported PID belongs to our systemd service.
func checkPort(port int) error {
	listeners, err := command("ss", "-ltnpH", fmt.Sprintf("sport = :%d", port))
	if err != nil {
		return err
	}
	if strings.TrimSpace(listeners) == "" {
		return nil
	}
	pid, err := command("systemctl", "show", "--property=MainPID", "--value", ServiceName)
	if err != nil {
		return err
	}
	pid = strings.TrimSpace(pid)
	processPID := regexp.MustCompile(`pid=(\d+)`)
	for _, line := range strings.Split(strings.TrimSpace(listeners), "\n") {
		matches := processPID.FindAllStringSubmatch(line, -1)
		if len(matches) == 0 {
			return fmt.Errorf("port %d is occupied by an unidentified process: %s", port, line)
		}
		for _, match := range matches {
			if pid == "0" || match[1] != pid {
				return fmt.Errorf("port %d is occupied by process: %s", port, line)
			}
		}
	}
	return nil
}

// reloadSSH falls back to sshd only when the distribution has no ssh.service unit.
func reloadSSH() error {
	state, err := command("systemctl", "show", "--property=LoadState", "--value", "ssh.service")
	if err != nil {
		return err
	}
	name := "ssh"
	if strings.TrimSpace(state) == "not-found" {
		name = "sshd"
	}
	_, err = command("systemctl", "reload", name)
	return err
}

// writeTemplate renders embedded units from the single installation settings file.
func writeTemplate(name, path string, port int) error {
	t, err := template.ParseFS(units, "units/"+name)
	if err != nil {
		return err
	}
	var rendered bytes.Buffer
	settings := map[string]any{"Port": port, "BinaryPath": BinaryPath, "ConfigDir": ConfigDir, "DataDir": DataDir, "BackupDir": BackupDir, "SystemUser": SystemUser, "TunnelGroup": TunnelGroup}
	if err := t.Execute(&rendered, settings); err != nil {
		return err
	}
	return writeFile(path, rendered.Bytes(), 0644)
}

// writeFile atomically replaces a managed file with explicit root ownership and permissions.
func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".mailbox-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Chown(0, 0); err != nil {
		file.Close()
		return err
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

// removeFile makes removal of already absent managed files idempotent.
func removeFile(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// copyExecutable uses an atomic replacement so reinstalling never truncates a running binary.
func copyExecutable() error {
	source, err := os.Executable()
	if err != nil {
		return err
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	destination, err := os.Stat(BinaryPath)
	if err == nil && os.SameFile(info, destination) {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(BinaryPath), 0755); err != nil {
		return err
	}
	output, err := os.CreateTemp(filepath.Dir(BinaryPath), ".mailbox-*")
	if err != nil {
		return err
	}
	defer os.Remove(output.Name())
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	if err := output.Chown(0, 0); err != nil {
		output.Close()
		return err
	}
	if err := output.Chmod(0755); err != nil {
		output.Close()
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	return os.Rename(output.Name(), BinaryPath)
}
