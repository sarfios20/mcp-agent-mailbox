package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type DoctorCheck struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

type DoctorResult struct {
	OK     bool          `json:"ok"`
	Checks []DoctorCheck `json:"checks"`
}

// Doctor reports independent installation health checks without changing host state.
// All six checks run even when installation files or another check fail.
func Doctor() (DoctorResult, error) {
	if err := requireRoot(); err != nil {
		return DoctorResult{}, err
	}
	result := DoctorResult{OK: true, Checks: []DoctorCheck{}}
	add := func(name, detail string, err error) {
		check := DoctorCheck{Name: name, Pass: err == nil, Detail: detail}
		if err != nil {
			check.Detail = err.Error()
			result.OK = false
		}
		result.Checks = append(result.Checks, check)
	}
	unitPath := filepath.Join(UnitDir, ServiceName)
	info, installedErr := os.Stat(unitPath)
	if installedErr == nil && !info.Mode().IsRegular() {
		installedErr = fmt.Errorf("%s is not a regular unit file", unitPath)
	}
	add("installed", "unit file at "+unitPath, installedErr)

	configPath := filepath.Join(DataDir, "server.json")
	data, configErr := os.ReadFile(configPath)
	var config struct {
		Port int `json:"port"`
	}
	if configErr == nil {
		configErr = json.Unmarshal(data, &config)
	}
	if configErr == nil && (config.Port < 1 || config.Port > 65535) {
		configErr = fmt.Errorf("invalid server port %d", config.Port)
	}

	err := doctorActive(ServiceName)
	add("service_active", ServiceName+" is active", err)
	err = configErr
	if err == nil {
		err = doctorPortListening(config.Port)
	}
	add("port_listening", fmt.Sprintf("port %d is listening", config.Port), err)
	err = configErr
	if err == nil {
		err = doctorHostKey(config.Port)
	}
	add("host_key", "SSH host key matches the stored key", err)

	err = doctorActive(BackupTimerName)
	add("backup_timer_active", BackupTimerName+" is active", err)
	detail, err := doctorRecentBackup(unitPath)
	add("recent_backup", detail, err)
	return result, nil
}

// doctorActive checks a unit's active state while keeping command output in the report.
func doctorActive(unit string) error {
	state, err := command("systemctl", "show", "--property=ActiveState", "--value", unit)
	if err != nil {
		return err
	}
	if strings.TrimSpace(state) != "active" {
		return fmt.Errorf("%s is not active: %s", unit, strings.TrimSpace(state))
	}
	return nil
}

// doctorPortListening requires a TCP listener on the port recorded by serve.
func doctorPortListening(port int) error {
	listeners, err := command("ss", "-ltnH", fmt.Sprintf("sport = :%d", port))
	if err != nil {
		return err
	}
	if strings.TrimSpace(listeners) == "" {
		return fmt.Errorf("no listener on port %d", port)
	}
	return nil
}

// doctorHostKey compares the presented SSH key with the stored private key's public key.
// The callback aborts the handshake after comparison, before authentication is attempted.
func doctorHostKey(port int) error {
	privateKey, err := os.ReadFile(filepath.Join(DataDir, "host_ed25519_key"))
	if err != nil {
		return err
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return err
	}
	address := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	connection, err := net.DialTimeout("tcp", address, DoctorSSHTimeout)
	if err != nil {
		return err
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(DoctorSSHTimeout)); err != nil {
		return err
	}
	verified := errors.New("host key verified; stop before authentication")
	config := &ssh.ClientConfig{
		User: SystemUser,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if !bytes.Equal(key.Marshal(), signer.PublicKey().Marshal()) {
				return errors.New("SSH host key does not match the stored host key")
			}
			return verified
		},
	}
	_, _, _, err = ssh.NewClientConn(connection, address, config)
	if errors.Is(err, verified) {
		return nil
	}
	if err == nil {
		return errors.New("SSH handshake did not verify the host key")
	}
	return err
}

// doctorRecentBackup uses the newest SQLite file's mtime, or the unit file's mtime before the first backup.
func doctorRecentBackup(unitPath string) (string, error) {
	entries, err := os.ReadDir(BackupDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var newest time.Time
	var newestPath string
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sqlite") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return "", err
		}
		if info.Mode().IsRegular() && (newestPath == "" || info.ModTime().After(newest)) {
			newest = info.ModTime()
			newestPath = filepath.Join(BackupDir, entry.Name())
		}
	}
	if newestPath != "" {
		if time.Since(newest) >= DoctorBackupMaxAge {
			return "", fmt.Errorf("last backup %s is at least %s old", newestPath, DoctorBackupMaxAge)
		}
		return "recent backup: " + newestPath, nil
	}
	info, err := os.Stat(unitPath)
	if err != nil {
		return "", err
	}
	if time.Since(info.ModTime()) >= DoctorBackupMaxAge {
		return "", fmt.Errorf("no backups in %s; installed at least %s ago", BackupDir, DoctorBackupMaxAge)
	}
	return fmt.Sprintf("no backups yet; installed less than %s ago", DoctorBackupMaxAge), nil
}
