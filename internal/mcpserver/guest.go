package mcpserver

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/ZetGames/vm-harness/harness"
	"github.com/ZetGames/vm-harness/vm"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type credentials struct {
	User     string `json:"user,omitempty" jsonschema:"guest OS account used through VirtualBox Guest Additions or VMware Tools"`
	Password string `json:"password,omitempty" jsonschema:"password of the guest OS account"`
}

type sshOptions struct {
	User     string `json:"user,omitempty" jsonschema:"ssh user, default the user vmh created with cloud_init"`
	Password string `json:"password,omitempty" jsonschema:"ssh password"`
	KeyPath  string `json:"key_path,omitempty" jsonschema:"private key file on the host running vmh, default the key vmh generated"`
}

type guestAccess struct {
	credentials
	Transport string     `json:"transport,omitempty" jsonschema:"auto, guest or ssh, default auto: ssh when vmh holds an ssh key for the VM or ssh.key_path or ssh.password is set, guest tools otherwise and for Windows guests"`
	SSH       sshOptions `json:"ssh,omitzero" jsonschema:"ssh login overrides; the address is always the VM's port forward to guest port 22 or its guest ip"`
}

func (a guestAccess) access() harness.Access {
	return harness.Access{
		Credentials: vm.Credentials(a.credentials),
		Transport:   a.Transport,
		SSH:         harness.SSHOptions(a.SSH),
	}
}

type execInput struct {
	target
	Command    []string          `json:"command,omitempty" jsonschema:"program and arguments, run without a shell"`
	Script     string            `json:"script,omitempty" jsonschema:"shell script run with /bin/sh -c, or cmd.exe /c on Windows; use instead of command"`
	Env        map[string]string `json:"env,omitempty" jsonschema:"extra environment variables"`
	WorkDir    string            `json:"workdir,omitempty" jsonschema:"working directory in the guest"`
	TimeoutSec int               `json:"timeout_sec,omitempty" jsonschema:"stop the command after this many seconds, default no limit"`
	guestAccess
}

func (s *server) exec(ctx context.Context, in execInput) (vm.ExecResult, error) {
	acc := in.access()
	return s.m.Exec(ctx, in.ref(), harness.ExecRequest{
		ExecRequest: vm.ExecRequest{
			Command:     in.Command,
			Script:      in.Script,
			Env:         in.Env,
			WorkDir:     in.WorkDir,
			TimeoutSec:  in.TimeoutSec,
			Credentials: acc.Credentials,
		},
		Transport: acc.Transport,
		SSH:       acc.SSH,
	})
}

type copyInput struct {
	target
	Direction string `json:"direction" jsonschema:"to_guest or from_guest"`
	HostPath  string `json:"host_path" jsonschema:"file path on the host running vmh"`
	GuestPath string `json:"guest_path" jsonschema:"absolute file path in the guest"`
	guestAccess
}

func (s *server) copyFile(ctx context.Context, in copyInput) (*mcp.CallToolResult, error) {
	acc := in.access()
	req := harness.CopyRequest{
		CopyRequest: vm.CopyRequest{HostPath: in.HostPath, GuestPath: in.GuestPath, Credentials: acc.Credentials},
		Transport:   acc.Transport,
		SSH:         acc.SSH,
	}
	switch in.Direction {
	case "to_guest":
		if err := s.m.CopyTo(ctx, in.ref(), req); err != nil {
			return nil, err
		}
		return text(fmt.Sprintf("copied %s to %s in the guest", in.HostPath, in.GuestPath)), nil
	case "from_guest":
		if err := s.m.CopyFrom(ctx, in.ref(), req); err != nil {
			return nil, err
		}
		return text(fmt.Sprintf("copied %s in the guest to %s", in.GuestPath, in.HostPath)), nil
	}
	return nil, fmt.Errorf("unknown direction %q, want to_guest or from_guest: %w", in.Direction, vm.ErrInvalid)
}

type writeFileInput struct {
	target
	GuestPath string `json:"guest_path" jsonschema:"absolute file path in the guest"`
	Content   string `json:"content" jsonschema:"new content of the file"`
	Encoding  string `json:"encoding,omitempty" jsonschema:"base64 when content is base64-encoded binary data, omit for text"`
	guestAccess
}

func (s *server) writeFile(ctx context.Context, in writeFileInput) (*mcp.CallToolResult, error) {
	data, err := decodeContent(in.Content, in.Encoding)
	if err != nil {
		return nil, err
	}
	if err := s.m.WriteFile(ctx, in.ref(), in.GuestPath, data, in.access()); err != nil {
		return nil, err
	}
	return text(fmt.Sprintf("wrote %d bytes to %s", len(data), in.GuestPath)), nil
}

func decodeContent(content, encoding string) ([]byte, error) {
	switch encoding {
	case "":
		return []byte(content), nil
	case "base64":
		data, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return nil, fmt.Errorf("content is not valid base64: %v: %w", err, vm.ErrInvalid)
		}
		return data, nil
	}
	return nil, fmt.Errorf("unknown encoding %q, want base64 or none for text: %w", encoding, vm.ErrInvalid)
}

type readFileInput struct {
	target
	GuestPath string `json:"guest_path" jsonschema:"absolute file path in the guest"`
	guestAccess
}

type fileContent struct {
	Path     string `json:"path"`
	Size     int    `json:"size"`
	Encoding string `json:"encoding,omitempty"`
	Content  string `json:"content"`
}

func (s *server) readFile(ctx context.Context, in readFileInput) (fileContent, error) {
	data, err := s.m.ReadFile(ctx, in.ref(), in.GuestPath, in.access())
	if err != nil {
		return fileContent{}, err
	}
	out := fileContent{Path: in.GuestPath, Size: len(data)}
	if utf8.Valid(data) {
		out.Content = string(data)
	} else {
		out.Encoding = "base64"
		out.Content = base64.StdEncoding.EncodeToString(data)
	}
	return out, nil
}

type ipOutput struct {
	IP string `json:"ip"`
}

func (s *server) ip(ctx context.Context, in target) (ipOutput, error) {
	ip, err := s.m.GuestIP(ctx, in.ref())
	return ipOutput{IP: ip}, err
}

type waitInput struct {
	target
	For        string `json:"for" jsonschema:"running, stopped, paused, saved, ip, ssh or guest"`
	TimeoutSec int    `json:"timeout_sec,omitempty" jsonschema:"give up after this many seconds, default 300"`
	credentials
	SSH sshOptions `json:"ssh,omitzero" jsonschema:"ssh login overrides, used when for is ssh"`
}

func (s *server) wait(ctx context.Context, in waitInput) (harness.WaitResult, error) {
	return s.m.Wait(ctx, in.ref(), harness.WaitRequest{
		For:     in.For,
		Timeout: time.Duration(in.TimeoutSec) * time.Second,
		Access:  harness.Access{Credentials: vm.Credentials(in.credentials), SSH: harness.SSHOptions(in.SSH)},
	})
}

type screenshotInput struct {
	target
	credentials
}

func (s *server) screenshot(ctx context.Context, in screenshotInput) (*mcp.CallToolResult, error) {
	png, err := s.m.Screenshot(ctx, in.ref(), vm.Credentials(in.credentials))
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: png, MIMEType: "image/png"}}}, nil
}
