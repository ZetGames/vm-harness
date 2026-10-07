package sshexec

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	testUser     = "vmh"
	testPassword = "secret"
)

type testServer struct {
	t                   *testing.T
	listener            net.Listener
	password            string
	keyboardInteractive bool
	authorizedKey       ssh.PublicKey

	mu       sync.Mutex
	hostKey  ssh.Signer
	conns    []net.Conn
	commands []string
	signals  []string
	files    map[string][]byte
}

func newTestServer(t *testing.T) *testServer {
	return &testServer{
		t:        t,
		password: testPassword,
		hostKey:  newHostKey(t),
		files:    map[string][]byte{},
	}
}

func newHostKey(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func (s *testServer) start() *testServer {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.t.Fatal(err)
	}
	s.listener = l
	s.t.Cleanup(s.close)
	go s.serve()
	return s
}

func (s *testServer) close() {
	s.listener.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		c.Close()
	}
}

func (s *testServer) dialConfig() Config {
	addr := s.listener.Addr().(*net.TCPAddr)
	return Config{
		Host:        addr.IP.String(),
		Port:        addr.Port,
		User:        testUser,
		Password:    testPassword,
		DialTimeout: 5 * time.Second,
	}
}

func (s *testServer) setHostKey(key ssh.Signer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hostKey = key
}

func (s *testServer) publicHostKey() ssh.PublicKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hostKey.PublicKey()
}

func (s *testServer) lastCommand() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.commands) == 0 {
		return ""
	}
	return s.commands[len(s.commands)-1]
}

func (s *testServer) receivedSignal(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.signals, name)
}

func (s *testServer) file(name string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.files[name]
	return data, ok
}

func (s *testServer) setFile(name string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[name] = data
}

func (s *testServer) serverConfig() *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{}
	if s.password != "" && s.keyboardInteractive {
		cfg.KeyboardInteractiveCallback = func(conn ssh.ConnMetadata, challenge ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			answers, err := challenge(conn.User(), "", []string{"Password: "}, []bool{false})
			if err != nil {
				return nil, err
			}
			if len(answers) != 1 {
				return nil, errors.New("expected one answer")
			}
			return s.checkPassword(conn.User(), answers[0])
		}
	}
	if s.password != "" && !s.keyboardInteractive {
		cfg.PasswordCallback = func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			return s.checkPassword(conn.User(), string(password))
		}
	}
	if s.authorizedKey != nil {
		cfg.PublicKeyCallback = func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if conn.User() != testUser || !bytes.Equal(key.Marshal(), s.authorizedKey.Marshal()) {
				return nil, errors.New("key not authorized")
			}
			return nil, nil
		}
	}
	s.mu.Lock()
	cfg.AddHostKey(s.hostKey)
	s.mu.Unlock()
	return cfg
}

func (s *testServer) checkPassword(user, password string) (*ssh.Permissions, error) {
	if user != testUser || password != s.password {
		return nil, errors.New("wrong password")
	}
	return nil, nil
}

func (s *testServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, conn)
		s.mu.Unlock()
		go s.handleConn(conn)
	}
}

func (s *testServer) handleConn(conn net.Conn) {
	defer conn.Close()
	_, chans, reqs, err := ssh.NewServerConn(conn, s.serverConfig())
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "only sessions are supported")
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			return
		}
		go s.handleSession(ch, chReqs)
	}
}

func (s *testServer) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	gone := make(chan struct{})
	defer close(gone)
	for req := range reqs {
		switch req.Type {
		case "exec":
			var msg struct{ Command string }
			if err := ssh.Unmarshal(req.Payload, &msg); err != nil {
				req.Reply(false, nil)
				continue
			}
			s.mu.Lock()
			s.commands = append(s.commands, msg.Command)
			s.mu.Unlock()
			req.Reply(true, nil)
			go s.runCommand(ch, msg.Command, gone)
		case "signal":
			var msg struct{ Signal string }
			if err := ssh.Unmarshal(req.Payload, &msg); err == nil {
				s.mu.Lock()
				s.signals = append(s.signals, msg.Signal)
				s.mu.Unlock()
			}
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

func (s *testServer) runCommand(ch ssh.Channel, cmd string, gone <-chan struct{}) {
	defer ch.Close()
	status, exited := s.shell(cmd, ch, ch, ch.Stderr(), gone)
	if exited {
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
	}
}

func (s *testServer) shell(cmd string, stdin io.Reader, stdout, stderr io.Writer, gone <-chan struct{}) (int, bool) {
	if mkdir, target, ok := strings.Cut(cmd, " && cat > "); ok && strings.HasPrefix(mkdir, "mkdir -p ") {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return 1, true
		}
		s.setFile(unquote(target), data)
		return 0, true
	}
	if name, ok := strings.CutPrefix(cmd, "cat "); ok {
		if unquote(name) == "/dev/zero" {
			zeros := make([]byte, 32<<10)
			for {
				if _, err := stdout.Write(zeros); err != nil {
					return 0, false
				}
			}
		}
		data, found := s.file(unquote(name))
		if !found {
			fmt.Fprintf(stderr, "cat: %s: No such file or directory\n", unquote(name))
			return 1, true
		}
		stdout.Write(data)
		return 0, true
	}
	switch cmd {
	case "true":
		return 0, true
	case "fail":
		io.WriteString(stdout, "partial output\n")
		io.WriteString(stderr, "something broke\n")
		return 3, true
	case "flood":
		io.WriteString(stdout, "a"+strings.Repeat("é", 5<<19))
		io.WriteString(stderr, strings.Repeat("e", 2<<20))
		return 0, true
	case "sleep":
		<-gone
		return 0, false
	case "vanish":
		return 0, false
	default:
		io.WriteString(stdout, cmd)
		return 0, true
	}
}

func unquote(s string) string {
	if len(s) >= 2 && strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'") {
		return strings.ReplaceAll(s[1:len(s)-1], `'\''`, "'")
	}
	return s
}
