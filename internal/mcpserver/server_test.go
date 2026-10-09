package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/ZetGames/vm-harness/harness"
	"github.com/ZetGames/vm-harness/internal/memprovider"
	"github.com/ZetGames/vm-harness/vm"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const stdioServerEnv = "VMH_MCPSERVER_STDIO"

func TestMain(m *testing.M) {
	if os.Getenv(stdioServerEnv) == "1" {
		os.Exit(serveStdio())
	}
	os.Exit(m.Run())
}

func serveStdio() int {
	p := memprovider.New(vm.VirtualBox)
	p.Put(managed("web", vm.StateRunning))
	m := harness.New(harness.Config{Root: os.TempDir()}, p)
	if err := Run(context.Background(), m, "1.2.3", os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

type fixture struct {
	cs   *mcp.ClientSession
	vbox *memprovider.Provider
	vmw  *memprovider.Provider
	root string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	vbox := memprovider.New(vm.VirtualBox)
	vmw := memprovider.New(vm.VMware)
	vmw.Features = []string{vm.FeatureGuestExec, vm.FeatureGuestCopy, vm.FeatureScreenshot, vm.FeatureSnapshots, vm.FeatureLinkedClone}

	web := managed("web", vm.StateRunning)
	web.NICs = []vm.NIC{{Mode: vm.NetNAT}}
	web.PortForwards = []vm.PortForward{{Name: "http", Protocol: "tcp", HostIP: "127.0.0.1", HostPort: 18080, GuestPort: 80}}
	vbox.Put(web)
	vbox.Put(managed("db", vm.StateStopped))
	vbox.Put(vm.Machine{Name: "legacy", State: vm.StateRunning})
	vbox.Put(managed("shared", vm.StateStopped))
	vmw.Put(managed("desk", vm.StateRunning))
	vmw.Put(managed("shared", vm.StateStopped))

	root := t.TempDir()
	server := New(harness.New(harness.Config{Root: root}, vbox, vmw), "test")
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cs.Close()
		ss.Wait()
	})
	return &fixture{cs: cs, vbox: vbox, vmw: vmw, root: root}
}

func managed(name string, state vm.State) vm.Machine {
	return vm.Machine{Name: name, State: state, Managed: true}
}

func (f *fixture) call(t *testing.T, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := f.cs.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s %v: %v", tool, args, err)
	}
	return res
}

func callOK[T any](t *testing.T, f *fixture, tool string, args map[string]any) T {
	t.Helper()
	res := f.call(t, tool, args)
	if res.IsError {
		t.Fatalf("%s %v: %s", tool, args, resultText(t, res))
	}
	var out T
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: decode %s: %v", tool, raw, err)
	}
	return out
}

func (f *fixture) callText(t *testing.T, tool string, args map[string]any) string {
	t.Helper()
	res := f.call(t, tool, args)
	if res.IsError {
		t.Fatalf("%s %v: %s", tool, args, resultText(t, res))
	}
	return resultText(t, res)
}

func (f *fixture) callErr(t *testing.T, tool string, args map[string]any, code string) string {
	t.Helper()
	res := f.call(t, tool, args)
	msg := resultText(t, res)
	if !res.IsError {
		t.Fatalf("%s %v succeeded with %s, want a %s error", tool, args, msg, code)
	}
	rest, ok := strings.CutPrefix(msg, code+": ")
	if !ok {
		t.Fatalf("%s %v: error %q does not start with %q", tool, args, msg, code+": ")
	}
	if words := strings.ReplaceAll(code, "_", " "); strings.HasPrefix(rest, words) || strings.HasSuffix(rest, words) {
		t.Fatalf("%s %v: error %q repeats its code", tool, args, msg)
	}
	if res.StructuredContent != nil {
		t.Fatalf("%s: error result carries structured content %v", tool, res.StructuredContent)
	}
	return msg
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("result has %d content blocks, want 1", len(res.Content))
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T, want text", res.Content[0])
	}
	return tc.Text
}

func TestListTools(t *testing.T) {
	type expectation struct {
		required    []string
		readOnly    bool
		destructive bool
		idempotent  bool
		structured  bool
	}
	want := map[string]expectation{
		"vm_providers":  {readOnly: true, idempotent: true, structured: true},
		"vm_list":       {readOnly: true, idempotent: true, structured: true},
		"vm_get":        {required: []string{"vm"}, readOnly: true, idempotent: true, structured: true},
		"vm_create":     {required: []string{"name"}, structured: true},
		"vm_power":      {required: []string{"vm", "action"}, destructive: true, structured: true},
		"vm_delete":     {required: []string{"vm"}, destructive: true, idempotent: true},
		"vm_update":     {required: []string{"vm"}, idempotent: true, structured: true},
		"vm_clone":      {required: []string{"vm", "name"}, structured: true},
		"vm_snapshot":   {required: []string{"vm", "action"}, destructive: true, structured: true},
		"vm_exec":       {required: []string{"vm"}, destructive: true, structured: true},
		"vm_copy":       {required: []string{"vm", "direction", "host_path", "guest_path"}, destructive: true, idempotent: true},
		"vm_write_file": {required: []string{"vm", "guest_path", "content"}, destructive: true, idempotent: true},
		"vm_read_file":  {required: []string{"vm", "guest_path"}, readOnly: true, idempotent: true, structured: true},
		"vm_ip":         {required: []string{"vm"}, readOnly: true, idempotent: true, structured: true},
		"vm_wait":       {required: []string{"vm", "for"}, destructive: true, structured: true},
		"vm_screenshot": {required: []string{"vm"}, readOnly: true, idempotent: true},
		"vm_port":       {required: []string{"vm", "action"}, destructive: true, structured: true},
	}
	f := newFixture(t)
	seen := map[string]bool{}
	for tool, err := range f.cs.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		seen[tool.Name] = true
		exp, ok := want[tool.Name]
		if !ok {
			t.Errorf("unexpected tool %s", tool.Name)
			continue
		}
		if tool.Description == "" || strings.Contains(tool.Description, "\n") {
			t.Errorf("%s: description %q is not a single non-empty line", tool.Name, tool.Description)
		}
		schema := tool.InputSchema.(map[string]any)
		if got := requiredFields(schema); !slices.Equal(got, exp.required) {
			t.Errorf("%s: required = %v, want %v", tool.Name, got, exp.required)
		}
		if schema["additionalProperties"] != false {
			t.Errorf("%s: unknown arguments are accepted", tool.Name)
		}
		props, _ := schema["properties"].(map[string]any)
		if _, ok := props["provider"]; !ok && tool.Name != "vm_providers" {
			t.Errorf("%s: no provider argument", tool.Name)
		}
		a := tool.Annotations
		if a == nil {
			t.Errorf("%s: no annotations", tool.Name)
			continue
		}
		destructive := a.DestructiveHint != nil && *a.DestructiveHint
		if a.ReadOnlyHint != exp.readOnly || destructive != exp.destructive || a.IdempotentHint != exp.idempotent {
			t.Errorf("%s: readOnly=%v destructive=%v idempotent=%v, want %v %v %v", tool.Name,
				a.ReadOnlyHint, destructive, a.IdempotentHint, exp.readOnly, exp.destructive, exp.idempotent)
		}
		if !a.ReadOnlyHint && a.DestructiveHint == nil {
			t.Errorf("%s: destructive hint left to the client default", tool.Name)
		}
		if a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Errorf("%s: open world hint is not false", tool.Name)
		}
		if structured := tool.OutputSchema != nil; structured != exp.structured {
			t.Errorf("%s: has output schema = %v, want %v", tool.Name, structured, exp.structured)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("tool %s is not listed", name)
		}
	}
}

func requiredFields(schema map[string]any) []string {
	list, _ := schema["required"].([]any)
	var out []string
	for _, v := range list {
		out = append(out, v.(string))
	}
	return out
}

func TestCreateDescribesEveryOSType(t *testing.T) {
	f := newFixture(t)
	res, err := f.cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(res.Tools, func(tool *mcp.Tool) bool { return tool.Name == "vm_create" })
	if i < 0 {
		t.Fatal("vm_create is not listed")
	}
	props := res.Tools[i].InputSchema.(map[string]any)["properties"].(map[string]any)
	desc := props["os_type"].(map[string]any)["description"].(string)
	for _, name := range vm.OSTypes() {
		if !strings.Contains(desc, name) {
			t.Errorf("os_type description does not mention %s", name)
		}
	}
}

func TestToolError(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("vm %q was not created by vmh: %w", "legacy", vm.ErrForbidden), `forbidden: vm "legacy" was not created by vmh`},
		{fmt.Errorf("get: %w", fmt.Errorf("machine %q: %w", "x", vm.ErrNotFound)), `not_found: get: machine "x"`},
		{fmt.Errorf("%w: ssh 127.0.0.1:2222: connection refused", vm.ErrNotReady), "not_ready: ssh 127.0.0.1:2222: connection refused"},
		{fmt.Errorf("ssh 127.0.0.1:2222: %w: %w", io.EOF, vm.ErrNotReady), "not_ready: ssh 127.0.0.1:2222: EOF"},
		{fmt.Errorf("%w: %w", vm.ErrInvalid, errors.New(`json: unknown field "x"`)), `invalid_argument: json: unknown field "x"`},
		{fmt.Errorf("timed out after 1s waiting for vm %q to be running: %w", "db", context.DeadlineExceeded), `timeout: timed out after 1s waiting for vm "db" to be running`},
		{fmt.Errorf("wait: %w", context.Canceled), "canceled: wait"},
		{&vm.CommandError{Path: "VBoxManage", Args: []string{"showvminfo"}, ExitCode: 1, Stderr: "no such vm", Kind: vm.ErrNotFound}, "not_found: VBoxManage showvminfo: not found (exit 1): no such vm"},
		{vm.ErrNotFound, "not_found: not found"},
		{fmt.Errorf("read: %w", io.EOF), "internal: read: EOF"},
	}
	for _, c := range cases {
		if got := toolError(c.err).Error(); got != c.want {
			t.Errorf("toolError(%q) = %q, want %q", c.err, got, c.want)
		}
	}
	if toolError(nil) != nil {
		t.Error("toolError(nil) is not nil")
	}
}

func TestInstructionsListEveryErrorCode(t *testing.T) {
	for _, err := range []error{
		vm.ErrNotFound, vm.ErrExists, vm.ErrInvalidState, vm.ErrInvalid, vm.ErrUnsupported, vm.ErrNotReady,
		vm.ErrUnavailable, vm.ErrForbidden, vm.ErrLimit, context.DeadlineExceeded, context.Canceled, errors.New("boom"),
	} {
		if code := vm.Code(err); !strings.Contains(instructions, code) {
			t.Errorf("instructions do not mention the error code %s", code)
		}
	}
}

func TestDescriptions(t *testing.T) {
	f := newFixture(t)
	texts := map[string]string{"instructions": instructions}
	for tool, err := range f.cs.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		texts[tool.Name] = tool.Description
	}
	for name, text := range texts {
		lower := strings.ToLower(text)
		if strings.Contains(lower, "adopt") || strings.Contains(lower, "allow_unmanaged") || strings.Contains(lower, "works from any vm") {
			t.Errorf("%s tells agents how to use VMs vmh did not create: %q", name, text)
		}
	}
	mentions := map[string][]string{
		"instructions": {"never changes, clones", "can take minutes"},
		"vm_get":       {"ssh", "key_path", "console_log"},
		"vm_create":    {"cloud_init needs one of them", "can take minutes", "single-VM OVA or OVF", "limited to host_dirs"},
		"vm_copy":      {"never writes hypervisor files", "vmh root other than <root>/files"},
		"vm_port":      {"loopback address or an address of this host"},
		"vm_update":    {"labels in any power state"},
		"vm_clone":     {"cannot be cloned", "can take minutes"},
		"vm_snapshot":  {"can take minutes"},
		"vm_exec":      {"invalid_state"},
		"vm_wait":      {"can take minutes", "shorter timeout_sec", "no user given", "rejected guest login is retried", "may hard-reset a stuck VM that vmh manages", "at most twice per call", "recoveries", "Windows guests are never reset", "fails at once with not_ready", "another call would reset the VM again"},
		"vm_providers": {"warnings", "Hyper-V", "max_reliable_cpus", "gives an appliance that many"},
		"vm_ip":        {"DHCP", "since its current power-on", "a reset or a reboot inside the guest is not a new power-on"},
	}
	for name, phrases := range mentions {
		for _, phrase := range phrases {
			if !strings.Contains(texts[name], phrase) {
				t.Errorf("%s does not mention %q: %q", name, phrase, texts[name])
			}
		}
	}
}

func TestPropertyDescriptions(t *testing.T) {
	f := newFixture(t)
	schemas := map[string]map[string]any{}
	for tool, err := range f.cs.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		schemas[tool.Name] = tool.InputSchema.(map[string]any)
	}
	cases := []struct {
		tool string
		path []string
		want []string
	}{
		{"vm_create", []string{"appliance"}, []string{"single VM", "serial ports, shared folders and remote display", "only .ova files"}},
		{"vm_create", []string{"iso"}, []string{"plain ISO 9660 file"}},
		{"vm_create", []string{"cpus"}, []string{"max_reliable_cpus", "1 for VirtualBox on Hyper-V hosts", "an appliance gets that many"}},
		{"vm_create", []string{"nics", "model"}, []string{"virtio when omitted"}},
		{"vm_create", []string{"disk_image"}, []string{"not a VMDK descriptor or a differencing disk"}},
		{"vm_create", []string{"port_forwards", "host_ip"}, []string{"a loopback address or an address of this host"}},
		{"vm_create", []string{"shared_folders", "read_only"}, []string{"folder of a VM vmh does not manage", "only <root>/files may be shared writable"}},
		{"vm_port", []string{"host_ip"}, []string{"a loopback address or an address of this host"}},
	}
	for _, c := range cases {
		desc := propertyDescription(schemas[c.tool], c.path...)
		for _, phrase := range c.want {
			if !strings.Contains(desc, phrase) {
				t.Errorf("%s %s does not mention %q: %q", c.tool, strings.Join(c.path, "."), phrase, desc)
			}
		}
	}
}

func propertyDescription(schema map[string]any, path ...string) string {
	for _, name := range path {
		props, _ := schema["properties"].(map[string]any)
		schema, _ = props[name].(map[string]any)
		if items, ok := schema["items"].(map[string]any); ok {
			schema = items
		}
	}
	desc, _ := schema["description"].(string)
	return desc
}

func TestBadArgumentsAreProtocolErrors(t *testing.T) {
	f := newFixture(t)
	for _, args := range []map[string]any{
		{"provider": "virtualbox"},
		{"vm": "web", "colour": "red"},
		{"vm": 7},
	} {
		if res, err := f.cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "vm_get", Arguments: args}); err == nil {
			t.Errorf("vm_get %v = %+v, want a protocol error", args, res)
		}
	}
}

func TestRunServesStdio(t *testing.T) {
	var stderr strings.Builder
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), stdioServerEnv+"=1")
	cmd.Stderr = &stderr
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(t.Context(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	info := cs.InitializeResult()
	if info.ServerInfo.Name != "vmh" || info.ServerInfo.Version != "1.2.3" || info.Instructions == "" {
		t.Errorf("initialize result = %+v", info)
	}
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "vm_get", Arguments: map[string]any{"vm": "web"}})
	switch {
	case err != nil:
		t.Error(err)
	case res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, `"name":"web"`):
		t.Errorf("vm_get over stdio = %+v", res.Content[0])
	}
	if err := cs.Close(); err != nil {
		t.Errorf("server did not exit cleanly: %v: %s", err, stderr.String())
	}
}

const (
	initializeRequest = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`
	initialized       = `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`
)

func TestClientDisconnectEndsServeCleanly(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: "vmh", Version: "test"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "ping_client"}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		if err := req.Session.Ping(ctx, nil); err == nil {
			t.Error("ping answered by a client that closed its output")
		}
		return text("done"), nil, nil
	})
	input := strings.Join([]string{
		initializeRequest,
		initialized,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ping_client","arguments":{}}}`,
	}, "\n") + "\n"
	if err := serve(t.Context(), s, strings.NewReader(input), io.Discard); err != nil {
		t.Errorf("serve after the client closed its output: %v", err)
	}
}

func TestServeReportsBrokenInput(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: "vmh", Version: "test"}, nil)
	if err := serve(t.Context(), s, strings.NewReader("hello\n"), io.Discard); err == nil {
		t.Error("serve accepted input that is not JSON-RPC")
	}
}

func TestServeStopsOnCancel(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: "vmh", Version: "test"}, nil)
	in, w := io.Pipe()
	defer w.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, s, in, io.Discard) }()
	if _, err := io.WriteString(w, initializeRequest+"\n"); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("serve after cancel: %v", err)
	}
}
