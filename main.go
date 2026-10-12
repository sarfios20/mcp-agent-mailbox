package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"github.com/sarfios20/mcp-agent-mailbox/admin"
	"github.com/sarfios20/mcp-agent-mailbox/mailbox"
)

// main dispatches non-interactive subcommands and emits one JSON result for administration.
func main() {
	os.Exit(run(os.Args[1:]))
}

// run validates subcommand arguments and returns the CLI exit status.
func run(args []string) int {
	usage := func() int {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]any{"error": "usage: mcp-agent-mailbox serve [--data DIR] [--port N] | install [--port N] | add <name> --pubkey <file|-> --host <ssh-host> [--data DIR] | remove <name> [--data DIR] | doctor | backup | uninstall", "detail": nil})
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	var result any
	var err error
	exitCode := 0
	switch args[0] {
	case "serve":
		flags := flag.NewFlagSet("serve", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		data := flags.String("data", defaultDataDir, "data directory")
		port := flags.Int("port", defaultPort, "SSH port")
		if flags.Parse(args[1:]) != nil || flags.NArg() != 0 || *port < 1 || *port > 65535 {
			return usage()
		}
		err = serve(*data, *port)
		if err == nil {
			return 0
		}
	case "install":
		flags := flag.NewFlagSet("install", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		port := flags.Int("port", defaultPort, "SSH port")
		if flags.Parse(args[1:]) != nil || flags.NArg() != 0 || *port < 1 || *port > 65535 {
			return usage()
		}
		result, err = admin.Install(*port)
	case "add":
		if len(args) < 2 {
			return usage()
		}
		flags := flag.NewFlagSet("add", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		pubkey := flags.String("pubkey", "", "OpenSSH public key file or - for stdin")
		host := flags.String("host", "", "SSH host")
		data := flags.String("data", defaultDataDir, "data directory")
		name := args[1]
		if flags.Parse(args[2:]) != nil || flags.NArg() != 0 {
			return usage()
		}
		if *pubkey == "" || *host == "" {
			return usage()
		}
		result, err = admin.Add(name, *pubkey, *host, *data)
	case "remove":
		if len(args) < 2 {
			return usage()
		}
		flags := flag.NewFlagSet("remove", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		data := flags.String("data", defaultDataDir, "data directory")
		if flags.Parse(args[2:]) != nil || flags.NArg() != 0 {
			return usage()
		}
		result, err = admin.Remove(args[1], *data)
	case "doctor":
		if len(args) != 1 {
			return usage()
		}
		var diagnosis admin.DoctorResult
		diagnosis, err = admin.Doctor()
		result = diagnosis
		if !diagnosis.OK {
			exitCode = 1
		}
	case "backup":
		if len(args) != 1 {
			return usage()
		}
		result, err = admin.Backup()
	case "uninstall":
		if len(args) != 1 {
			return usage()
		}
		result, err = admin.Uninstall()
	default:
		return usage()
	}
	if err != nil {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]any{"error": err.Error(), "detail": nil})
		return 1
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
	return exitCode
}

// serve opens the data directory and serves MCP over its own SSH listener.
func serve(dataDir string, port int) error {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return err
	}
	if err := admin.BecomeDataOwner(dataDir); err != nil {
		return err
	}
	store, err := mailbox.Open(filepath.Join(dataDir, "mailbox.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	server, err := newSSHServer(store, dataDir)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	defer listener.Close()
	data, err := json.Marshal(map[string]int{"port": port})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dataDir, "server.json"), append(data, '\n'), 0600); err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"serving": true, "port": port, "data": dataDir}); err != nil {
		return err
	}
	return server.Serve(listener)
}
