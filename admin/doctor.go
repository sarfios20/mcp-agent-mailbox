package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
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
// Only a missing installation skips the remaining checks; root failures use the CLI error convention.
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
	configPath := filepath.Join(ConfigDir, "config.json")
	data, configErr := os.ReadFile(configPath)
	if errors.Is(configErr, os.ErrNotExist) {
		add("installed", "", errors.New("not installed: run install first"))
		return result, nil
	}
	var config struct {
		Port int `json:"port"`
	}
	if configErr == nil {
		configErr = json.Unmarshal(data, &config)
	}
	if configErr == nil && (config.Port < 1 || config.Port > 65535) {
		configErr = fmt.Errorf("invalid installed port %d", config.Port)
	}
	add("installed", fmt.Sprintf("configuration at %s; port %d", configPath, config.Port), configErr)

	err := doctorActive(ServiceName)
	add("service_active", ServiceName+" is active", err)
	err = configErr
	if err == nil {
		err = doctorLoopback(config.Port)
	}
	add("loopback_only", fmt.Sprintf("port %d listens only on loopback", config.Port), err)
	err = configErr
	if err == nil {
		err = doctorMissingToken(config.Port)
	}
	add("rejects_missing_token", "unauthenticated POST /mcp returned 401", err)

	info, dropInErr := os.Stat(DropInPath)
	if dropInErr == nil && !info.Mode().IsRegular() {
		dropInErr = fmt.Errorf("%s is not a regular file", DropInPath)
	}
	_, sshErr := command("sshd", "-t")
	add("sshd_dropin", DropInPath+" is present; sshd -t passed", errors.Join(dropInErr, sshErr))
	err = doctorActive(BackupTimerName)
	add("backup_timer_active", BackupTimerName+" is active", err)
	detail, err := doctorRecentBackup(configPath)
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

// doctorLoopback requires at least one listener on the installed port and rejects every non-loopback address.
func doctorLoopback(port int) error {
	listeners, err := command("ss", "-ltnH", fmt.Sprintf("sport = :%d", port))
	if err != nil {
		return err
	}
	if strings.TrimSpace(listeners) == "" {
		return fmt.Errorf("no listener on port %d", port)
	}
	for _, line := range strings.Split(strings.TrimSpace(listeners), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			return fmt.Errorf("invalid ss listener output: %q", line)
		}
		host, _, err := net.SplitHostPort(fields[3])
		if err != nil {
			return fmt.Errorf("invalid listener address %q: %w", fields[3], err)
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("non-loopback listener: %s", fields[3])
		}
	}
	return nil
}

// doctorMissingToken probes the local endpoint directly, without following redirects or using a proxy.
func doctorMissingToken(port int) error {
	client := &http.Client{
		Timeout:   DoctorHTTPTimeout,
		Transport: &http.Transport{},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()
	response, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/mcp", port), "application/json", nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("unauthenticated POST /mcp returned %d, expected 401", response.StatusCode)
	}
	return nil
}

// doctorRecentBackup uses the newest SQLite file's mtime, or config.json's mtime when no backup exists yet.
func doctorRecentBackup(configPath string) (string, error) {
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
	info, err := os.Stat(configPath)
	if err != nil {
		return "", err
	}
	if time.Since(info.ModTime()) >= DoctorBackupMaxAge {
		return "", fmt.Errorf("no backups in %s; installed at least %s ago", BackupDir, DoctorBackupMaxAge)
	}
	return fmt.Sprintf("no backups yet; installed less than %s ago", DoctorBackupMaxAge), nil
}
