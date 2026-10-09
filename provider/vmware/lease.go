package vmware

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ZetGames/vm-harness/vm"
)

const (
	leaseTimeLayout = "2006/01/02 15:04:05"
	vmwareLog       = "vmware.log"
	leaseSkew       = 5 * time.Second
)

type lease struct {
	ip      string
	mac     string
	starts  time.Time
	ends    time.Time
	endless bool
	unused  bool
}

func (l lease) current(now time.Time) bool {
	return l.endless || now.Before(l.ends)
}

func (p *Provider) leasedIP(path string) string {
	poweredOn := powerOnTime(path)
	if poweredOn.IsZero() {
		return ""
	}
	v, err := readVMX(path)
	if err != nil {
		return ""
	}
	var leases []lease
	for _, pattern := range p.leases {
		files, _ := filepath.Glob(pattern)
		for _, file := range files {
			if data, err := os.ReadFile(file); err == nil {
				leases = append(leases, parseLeases(string(data))...)
			}
		}
	}
	since, now := poweredOn.Add(-leaseSkew), time.Now()
	for _, nic := range nics(v) {
		if nic.Mode == vm.NetBridged || nic.MAC == "" {
			continue
		}
		if ip := newestLease(leases, nic.MAC, since, now); ip != "" {
			return ip
		}
	}
	return ""
}

func powerOnTime(path string) time.Time {
	f, err := os.Open(filepath.Join(filepath.Dir(path), vmwareLog))
	if err != nil {
		return time.Time{}
	}
	defer f.Close()
	head := make([]byte, 256)
	n, _ := io.ReadFull(f, head)
	fields := strings.Fields(string(head[:n]))
	if len(fields) == 0 {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSuffix(fields[0], "|"))
	if err != nil {
		return time.Time{}
	}
	return t
}

func newestLease(leases []lease, mac string, since, now time.Time) string {
	var best *lease
	for i := range leases {
		l := &leases[i]
		if l.unused || !strings.EqualFold(l.mac, mac) || !l.current(now) || l.starts.Before(since) {
			continue
		}
		if best == nil || !l.starts.Before(best.starts) {
			best = l
		}
	}
	if best == nil {
		return ""
	}
	return best.ip
}

func parseLeases(text string) []lease {
	var (
		leases []lease
		cur    *lease
		stmt   []string
		depth  int
	)
	for _, token := range leaseTokens(text) {
		switch token {
		case "{":
			depth++
			if depth == 1 && len(stmt) == 2 && stmt[0] == "lease" && net.ParseIP(stmt[1]) != nil {
				cur = &lease{ip: stmt[1]}
			}
			stmt = nil
		case "}":
			depth = max(depth-1, 0)
			if depth == 0 && cur != nil {
				leases = append(leases, *cur)
				cur = nil
			}
			stmt = nil
		case ";":
			if depth == 1 && cur != nil {
				cur.apply(stmt)
			}
			stmt = nil
		default:
			stmt = append(stmt, token)
		}
	}
	return lastPerIP(leases)
}

func (l *lease) apply(stmt []string) {
	switch {
	case len(stmt) == 3 && stmt[0] == "hardware" && stmt[1] == "ethernet":
		l.mac = stmt[2]
	case len(stmt) > 1 && stmt[0] == "starts":
		l.starts = leaseTime(stmt[1:])
	case len(stmt) == 2 && stmt[0] == "ends" && stmt[1] == "never":
		l.endless = true
	case len(stmt) > 1 && stmt[0] == "ends":
		l.ends = leaseTime(stmt[1:])
	case len(stmt) == 1 && stmt[0] == "abandoned",
		len(stmt) == 3 && stmt[0] == "binding" && stmt[1] == "state" && stmt[2] != "active" && stmt[2] != "expired":
		l.unused = true
	}
}

func leaseTime(fields []string) time.Time {
	if len(fields) != 3 {
		return time.Time{}
	}
	t, _ := time.Parse(leaseTimeLayout, fields[1]+" "+fields[2])
	return t
}

func lastPerIP(leases []lease) []lease {
	last := make(map[string]int, len(leases))
	for i, l := range leases {
		last[l.ip] = i
	}
	kept := leases[:0]
	for i, l := range leases {
		if last[l.ip] == i {
			kept = append(kept, l)
		}
	}
	return kept
}

func leaseTokens(text string) []string {
	var tokens []string
	for i := 0; i < len(text); {
		switch c := text[i]; {
		case c == '#':
			end := strings.IndexByte(text[i:], '\n')
			if end < 0 {
				return tokens
			}
			i += end
		case c == '{' || c == '}' || c == ';':
			tokens = append(tokens, text[i:i+1])
			i++
		case c == '"':
			j := i + 1
			for j < len(text) && text[j] != '"' {
				if text[j] == '\\' {
					j++
				}
				j++
			}
			j = min(j, len(text))
			tokens = append(tokens, text[i:j])
			i = j + 1
		case c <= ' ':
			i++
		default:
			j := i
			for j < len(text) && text[j] > ' ' && !strings.ContainsRune(`{};"#`, rune(text[j])) {
				j++
			}
			tokens = append(tokens, text[i:j])
			i = j
		}
	}
	return tokens
}
