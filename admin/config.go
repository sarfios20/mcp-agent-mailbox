// Package admin installs and removes the system service and SSH tunnel configuration.
package admin

// Installation settings are kept together; templates receive these same values.
const (
	// DefaultPort is the loopback TCP port used when install has no --port argument.
	DefaultPort = 8765
	// BinaryPath is the installed static executable.
	BinaryPath = "/usr/local/bin/mcp-agent-mailbox"
	// ConfigDir holds root-only tokens and the installed port.
	ConfigDir = "/etc/mcp-agent-mailbox"
	// DataDir is the persistent systemd state directory.
	DataDir = "/var/lib/mcp-agent-mailbox"
	// BackupDir holds verified backups retained across uninstall.
	BackupDir = "/var/backups/mcp-agent-mailbox"
	// DropInPath is the only SSH configuration file managed by this package.
	DropInPath = "/etc/ssh/sshd_config.d/10-mcp-agent-mailbox.conf"
	// KeysDir holds root-owned public keys outside tunnel users' home directories.
	KeysDir = "/etc/ssh/mcp-agent-mailbox-keys"
	// ParticipantNamePattern restricts names used in tokens and Unix account paths.
	ParticipantNamePattern = `^[a-z][a-z0-9-]{0,31}$`
	// TokenPattern requires a lowercase hex encoding of a 256-bit bearer token.
	TokenPattern = `^[0-9a-f]{64}$`
	// TokenBytes is the number of cryptographically random bytes in each token.
	TokenBytes = 32
	// SystemUser owns the mailbox data and runs the server.
	SystemUser = "mailbox"
	// TunnelGroup identifies restricted SSH tunnel accounts.
	TunnelGroup = "mailbox-tunnel"
	// UnitDir is systemd's administrator unit directory.
	UnitDir = "/etc/systemd/system"
	// SSHConfigPath must include the managed drop-in before any Match block.
	SSHConfigPath = "/etc/ssh/sshd_config"
	// OSReleasePath supplies the supported distribution and version.
	OSReleasePath = "/etc/os-release"
	// ServiceName identifies the mailbox process when checking an occupied port.
	ServiceName = "mcp-agent-mailbox.service"
	// BackupServiceName is the scheduled backup's oneshot unit.
	BackupServiceName = "mcp-agent-mailbox-backup.service"
	// BackupTimerName is the daily backup timer.
	BackupTimerName = "mcp-agent-mailbox-backup.timer"
)
