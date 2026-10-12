package admin

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"
)

//go:embed units/*
var units embed.FS

// Install provisions the mailbox user, owned data and backup directories, and systemd units.
// Repeated calls preserve data; reinstalling restarts an active server with the requested port.
func Install(port int) (any, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid port %d", port)
	}
	manual := fmt.Sprintf("run mcp-agent-mailbox serve --data %s --port %d by hand", DataDir, port)
	if err := requireRoot(); err != nil {
		return nil, fmt.Errorf("%w; %s", err, manual)
	}
	if info, err := os.Stat(SystemdRuntimeDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("install requires systemd; %s", manual)
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil, fmt.Errorf("install requires systemctl in PATH; %s", manual)
	}
	for _, tool := range []string{"ss", "useradd", "getent"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, fmt.Errorf("required command %s: %w", tool, err)
		}
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
	account, exists, err := lookup("passwd", SystemUser)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimSpace(account), ":")
	if !exists || len(fields) != 7 {
		return nil, fmt.Errorf("invalid getent passwd output for %s: %q", SystemUser, account)
	}
	uid, err := strconv.Atoi(fields[2])
	if err != nil {
		return nil, err
	}
	gid, err := strconv.Atoi(fields[3])
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{DataDir, BackupDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		if err := os.Chown(dir, uid, gid); err != nil {
			return nil, err
		}
		if err := os.Chmod(dir, 0700); err != nil {
			return nil, err
		}
	}
	for _, name := range []string{ServiceName, BackupServiceName, BackupTimerName} {
		if err := writeTemplate(name, filepath.Join(UnitDir, name), port); err != nil {
			return nil, err
		}
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
	if err := waitServing(port); err != nil {
		return nil, err
	}
	return map[string]any{"installed": true, "port": port, "binary": BinaryPath, "data_dir": DataDir, "backup_dir": BackupDir}, nil
}

// waitServing returns once the server has recorded port in server.json and presents
// the stored host key there, so doctor passes right after install. Fails after StartTimeout.
func waitServing(port int) error {
	deadline := time.Now().Add(StartTimeout)
	for {
		err := servingOn(port)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("server did not answer on port %d within %s (see journalctl -u %s): %w", port, StartTimeout, ServiceName, err)
		}
		time.Sleep(StartPollInterval)
	}
}

// servingOn checks one time that server.json names port and the host key matches there.
func servingOn(port int) error {
	data, err := os.ReadFile(filepath.Join(DataDir, "server.json"))
	if err != nil {
		return err
	}
	var config struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return err
	}
	if config.Port != port {
		return fmt.Errorf("server.json has port %d, want %d", config.Port, port)
	}
	return doctorHostKey(port)
}

// Uninstall removes the three managed units while preserving the mailbox user, data and backups.
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
	return map[string]any{"uninstalled": true, "kept": []string{DataDir, BackupDir, SystemUser}}, nil
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

// writeTemplate renders embedded units from the single installation settings file.
func writeTemplate(name, path string, port int) error {
	t, err := template.ParseFS(units, "units/"+name)
	if err != nil {
		return err
	}
	var rendered bytes.Buffer
	settings := map[string]any{"Port": port, "BinaryPath": BinaryPath, "DataDir": DataDir, "BackupDir": BackupDir, "SystemUser": SystemUser}
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
