package cli

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/fl4metf/vm-harness/harness"
	"github.com/fl4metf/vm-harness/internal/httpapi"
	"github.com/fl4metf/vm-harness/internal/mcpserver"
	"github.com/fl4metf/vm-harness/internal/secfile"
)

const serveHelp = `Serve the vmh REST API (JSON over HTTP, routes under /v1) until interrupted.

Every request except GET /healthz needs the header
"Authorization: Bearer <token>". The token comes from VMH_TOKEN or --token;
without one vmh generates a new random token at every start and writes it to
<root>/serve.token, readable only by the current user. Listening on anything
but a loopback address requires a token of your own. Prefer VMH_TOKEN to
--token, which other local users can see in the process list.

Host paths in requests (copies, shared folders, ISOs, disk images, appliances,
SSH keys) must lie under <root>/files or under one of the host_dirs set in
config.json. Disk images must be self-contained (no VMDK descriptor or
differencing disk), ISOs plain ISO 9660 images and appliances .ova files, since
the hypervisor would follow those to other host files. Inside <root> only
<root>/files is writable. Operations are logged to stderr.`

var serveHTTP = httpapi.ListenAndServe

func (a *app) serveCommand() *cobra.Command {
	var addr, token string
	cmd := &cobra.Command{
		Use:     "serve",
		Short:   "Serve the REST API over HTTP",
		Long:    serveHelp,
		GroupID: groupServers,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			token = cmp.Or(token, os.Getenv("VMH_TOKEN"))
			if token == "" && !loopback(addr) {
				return invalid("refusing to serve on %s without a token: set VMH_TOKEN or --token, or listen on a loopback address", addr)
			}
			cfg, err := a.serveConfig()
			if err != nil {
				return err
			}
			attrs := []any{"addr", addr, "host_dirs", cfg.HostDirs}
			if token == "" {
				path := filepath.Join(cfg.Root, "serve.token")
				if token, err = writeToken(path); err != nil {
					return fmt.Errorf("write the api token: %w", err)
				}
				attrs = append(attrs, "token_file", path)
			}
			cfg.Logger = slog.New(slog.NewTextHandler(a.stderr, nil))
			cfg.Logger.Info("serving the vmh api", attrs...)
			return serveHTTP(cmd.Context(), addr, httpapi.New(newManager(cfg), token))
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:8070", "listen `address`")
	cmd.Flags().StringVar(&token, "token", "", "require this bearer `token` (default $VMH_TOKEN, else a generated one)")
	return cmd
}

func (a *app) serveConfig() (harness.Config, error) {
	cfg, err := a.loadConfig()
	if err != nil {
		return cfg, err
	}
	if len(cfg.HostDirs) == 0 {
		files := cfg.FilesDir()
		if err := os.MkdirAll(files, 0o700); err != nil {
			return cfg, err
		}
		cfg.HostDirs = []string{files}
	}
	return cfg, nil
}

func writeToken(path string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := secfile.Restrict(path); err != nil {
		return "", err
	}
	key := make([]byte, 32)
	rand.Read(key)
	token := hex.EncodeToString(key)
	if _, err := f.WriteString(token); err != nil {
		return "", err
	}
	return token, f.Close()
}

func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (a *app) mcpCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve the Model Context Protocol on stdin and stdout",
		Long: `Run an MCP server on stdin and stdout that offers the VM operations as tools
(vm_list, vm_create, vm_exec, ...). Register it in an MCP client with the
command "vmh mcp". Logs and errors go to stderr; stdout carries only protocol
messages. The server exits with status 0 when the client closes stdin.`,
		GroupID: groupServers,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a.format = formatText
			m, err := a.open(slog.New(slog.NewTextHandler(a.stderr, nil)))
			if err != nil {
				return err
			}
			return mcpserver.Run(cmd.Context(), m, version, a.stdin, a.stdout)
		},
	}
}

type versionInfo struct {
	Version string `json:"version"`
}

func (a *app) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the vmh version",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return a.emit(versionInfo{Version: version}, func(w io.Writer) { fmt.Fprintf(w, "vmh %s\n", version) })
		},
	}
}
