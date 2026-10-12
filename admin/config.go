// Package admin manages the mailbox system service, participants and backups.
package admin

import "time"

// Installation settings are kept together; templates receive these same values.
const (
	// DefaultPort is the SSH TCP port used when install has no --port argument.
	DefaultPort = 2222
	// BinaryPath is the installed static executable.
	BinaryPath = "/usr/local/bin/mcp-agent-mailbox"
	// DataDir is the persistent systemd state directory.
	DataDir = "/var/lib/mcp-agent-mailbox"
	// BackupDir holds verified backups retained across uninstall.
	BackupDir = "/var/backups/mcp-agent-mailbox"
	// BackupLockName is the exclusive backup lock inside the owned data directory.
	BackupLockName = "backup.lock"
	// BackupDailyCount retains the newest snapshot in each of the latest distinct days.
	BackupDailyCount = 3
	// BackupWeeklyCount retains the newest snapshot in each of the latest distinct ISO weeks.
	BackupWeeklyCount = 2
	// BackupMonthlyCount retains the newest snapshot in each of the latest distinct months.
	BackupMonthlyCount = 12
	// BackupYearlyCount retains the newest snapshot in each of the latest distinct years.
	BackupYearlyCount = 1
	// ParticipantNamePattern restricts participant names.
	ParticipantNamePattern = `^[a-z][a-z0-9-]{0,31}$`
	// SystemUser owns the mailbox data and runs the server.
	SystemUser = "mailbox"
	// SystemdRuntimeDir indicates that systemd is running.
	SystemdRuntimeDir = "/run/systemd/system"
	// UnitDir is systemd's administrator unit directory.
	UnitDir = "/etc/systemd/system"
	// ServiceName identifies the mailbox process when checking an occupied port.
	ServiceName = "mcp-agent-mailbox.service"
	// BackupServiceName is the scheduled backup's oneshot unit.
	BackupServiceName = "mcp-agent-mailbox-backup.service"
	// BackupTimerName is the daily backup timer.
	BackupTimerName = "mcp-agent-mailbox-backup.timer"
	// DoctorBackupMaxAge limits backup age and the grace period after installation.
	DoctorBackupMaxAge = 48 * time.Hour
	// DoctorSSHTimeout bounds connection and handshake time for the host key check.
	DoctorSSHTimeout = 3 * time.Second
	// StartTimeout bounds how long install waits for the started server to answer SSH.
	StartTimeout = 10 * time.Second
	// StartPollInterval is the pause between install's checks that the server answers.
	StartPollInterval = 100 * time.Millisecond
)
