package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/fl4metf/vm-harness/vm"
)

const (
	bootStallWindow = 90 * time.Second
	maxBootResets   = 2
	consoleTail     = 256 << 10
	consoleLineMax  = 100
	markTail        = 64
	kernelBanner    = "Linux version "
)

var consoleEscape = regexp.MustCompile(`\x1b(\[[0-?]*[ -/]*[@-~]?|.?)`)

type bootMark struct {
	At     time.Time `json:"at"`
	Offset int64     `json:"offset"`
	Tail   []byte    `json:"tail,omitempty"`
}

type bootWatch struct {
	path   string
	marks  string
	window time.Duration
	own    bootMark
	size   int64
	grew   time.Time
	idle   time.Time
	held   bool
}

type bootLook struct {
	size    int64
	modTime time.Time
	markAt  time.Time
	booting bool
	reason  string
}

type bootScan struct {
	booting  bool
	finished bool
	failure  string
	last     string
}

func (m *Manager) watchBoot(p vm.Provider, mach vm.Machine, goal string) *bootWatch {
	if mach.ConsoleLog == "" || !mach.Managed || mach.IsWindowsGuest() || m.bootResets <= 0 {
		return nil
	}
	if goal != WaitIP && goal != WaitSSH && goal != WaitGuest {
		return nil
	}
	return &bootWatch{path: mach.ConsoleLog, marks: m.bootMarkPath(p, mach.ID), window: m.bootStall, size: -1}
}

func (m *Manager) recoverBoot(ctx context.Context, p vm.Provider, mach vm.Machine, w *bootWatch, done []string) (string, error) {
	seen := w.look(time.Now())
	if seen.reason == "" && !w.held {
		if seen.booting && !m.runningFree(ctx, p, mach) {
			w.hold()
		}
		return "", nil
	}
	unlock, err := m.tryLockVM(ctx, p, mach)
	if err != nil {
		w.hold()
		return "", nil
	}
	defer unlock()
	current, err := m.get(ctx, p, mach.ID)
	if err != nil || current.State != vm.StateRunning {
		w.hold()
		return "", nil
	}
	if w.held {
		w.held = false
		w.idle = time.Now()
		return "", nil
	}
	if len(done) >= m.bootResets {
		return "", m.bootFailed(ctx, p, current, seen.reason, done)
	}
	again := w.look(time.Now())
	if !current.Managed || current.IsWindowsGuest() || again.reason == "" || !again.same(seen) {
		return "", nil
	}
	start := time.Now()
	m.log.Warn("boot is stuck, resetting the vm", "vm", current.Name, "reason", again.reason)
	note := again.reason + " — reset"
	w.own, err = m.power(p, current, true, func() error { return p.Reset(ctx, current.ID) })
	if err != nil {
		note = fmt.Sprintf("%s — reset returned an error: %v", again.reason, err)
	}
	m.logOp(ctx, "reset", p.Name(), current.Name, time.Since(start), err)
	return note, nil
}

func (m *Manager) tryLockVM(ctx context.Context, p vm.Provider, mach vm.Machine) (func(), error) {
	busy, cancel := context.WithCancel(ctx)
	cancel()
	return m.lockVM(busy, p, mach)
}

func (m *Manager) runningFree(ctx context.Context, p vm.Provider, mach vm.Machine) bool {
	unlock, err := m.tryLockVM(ctx, p, mach)
	if err != nil {
		return false
	}
	unlock()
	current, err := m.get(ctx, p, mach.ID)
	return err == nil && current.State == vm.StateRunning
}

func (w *bootWatch) hold() {
	w.held = true
	w.idle = time.Now()
}

func (m *Manager) bootFailed(ctx context.Context, p vm.Provider, mach vm.Machine, reason string, done []string) error {
	hint := "see the console log " + mach.ConsoleLog
	if info, err := p.Info(ctx); err == nil && info.MaxReliableCPUs > 0 && mach.CPUs > info.MaxReliableCPUs {
		hint = fmt.Sprintf("this host boots %s VMs with more than %d vCPU unreliably, stop the vm and set cpus to %d", p.Name(), info.MaxReliableCPUs, info.MaxReliableCPUs)
	}
	return fmt.Errorf("boot of vm %q is stuck again%s: %s; this wait gives up, and another wait would reset the vm again, so fix the cause first (%s): %w", mach.Name, resetNote(done), reason, hint, vm.ErrNotReady)
}

func (m *Manager) power(p vm.Provider, mach vm.Machine, boots bool, call func() error) (bootMark, error) {
	if mach.ConsoleLog == "" {
		return bootMark{}, call()
	}
	path := m.bootMarkPath(p, mach.ID)
	mark := readMark(path)
	next := mark
	if boots {
		next = consoleMark(mach.ConsoleLog)
	}
	err := call()
	if err == nil {
		mark = next
	}
	mark.At = time.Now()
	data, _ := json.Marshal(mark)
	if werr := os.WriteFile(path, data, 0o600); werr != nil {
		m.log.Warn("record the boot of the vm", "vm", mach.Name, "error", werr)
	}
	return mark, err
}

func (m *Manager) bootMarkPath(p vm.Provider, id string) string {
	return filepath.Join(m.cfg.Root, "locks", p.Name()+"-"+idHash(id)+".boot")
}

func readMark(path string) bootMark {
	var mark bootMark
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &mark) != nil {
		return bootMark{}
	}
	return mark
}

func consoleMark(path string) bootMark {
	var mark bootMark
	f, err := os.Open(path)
	if err != nil {
		return mark
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return mark
	}
	tail := make([]byte, min(markTail, info.Size()))
	if _, err := f.ReadAt(tail, info.Size()-int64(len(tail))); err == nil {
		mark.Offset, mark.Tail = info.Size(), tail
	}
	return mark
}

func (mark bootMark) holds(f io.ReaderAt, size int64) bool {
	if mark.Offset > size || int64(len(mark.Tail)) != min(markTail, mark.Offset) {
		return false
	}
	tail := make([]byte, len(mark.Tail))
	_, err := f.ReadAt(tail, mark.Offset-int64(len(tail)))
	return err == nil && bytes.Equal(tail, mark.Tail)
}

func (w *bootWatch) look(now time.Time) bootLook {
	mark := readMark(w.marks)
	if w.own.At.After(mark.At) {
		mark = w.own
	}
	f, err := os.Open(w.path)
	if err != nil {
		return bootLook{}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return bootLook{}
	}
	look := bootLook{size: info.Size(), modTime: info.ModTime(), markAt: mark.At}
	if look.size != w.size {
		if w.size >= 0 {
			w.grew = now
		}
		w.size = look.size
	}
	from := max(0, look.size-consoleTail)
	if mark.holds(f, look.size) {
		from = max(from, mark.Offset)
	}
	buf := make([]byte, look.size-from)
	n, _ := f.ReadAt(buf, from)
	boot := scanBoot(string(buf[:n]))
	quiet := slices.MaxFunc([]time.Time{look.modTime, w.grew, w.idle, mark.At}, time.Time.Compare)
	look.booting = boot.booting && !boot.finished
	switch {
	case boot.failure != "":
		look.reason = boot.failure
	case boot.booting && !boot.finished && now.Sub(quiet) >= w.window:
		look.reason = fmt.Sprintf("boot stalled for %ds at: %s", int(w.window.Seconds()), boot.last)
	}
	return look
}

func (l bootLook) same(o bootLook) bool {
	return l.size == o.size && l.modTime.Equal(o.modTime) && l.markAt.Equal(o.markAt) && l.reason == o.reason
}

func scanBoot(text string) bootScan {
	var s bootScan
	if i := strings.LastIndex(text, kernelBanner); i >= 0 {
		s.booting = true
		text = text[i:]
	}
	lines := strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' })
	complete := len(lines)
	if !strings.HasSuffix(text, "\n") && !strings.HasSuffix(text, "\r") {
		complete--
	}
	for i, raw := range lines {
		line := consoleLine(raw)
		if line == "" {
			continue
		}
		s.last = line
		s.finished = s.finished || bootDone(line)
		if i < complete && s.failure == "" {
			s.failure = bootFailure(line)
		}
	}
	s.last = clip(s.last)
	return s
}

func bootDone(line string) bool {
	if strings.Contains(line, "login:") {
		return true
	}
	return strings.Contains(line, "Cloud-init v.") && strings.Contains(line, " finished")
}

func bootFailure(line string) string {
	switch {
	case strings.Contains(line, "Kernel panic"):
		return panicReason(line)
	case strings.Contains(line, "Invalid MAC Address"):
		return "network card has an invalid MAC address, no network this boot"
	}
	return ""
}

func consoleLine(raw string) string {
	return strings.TrimSpace(strings.Map(printable, consoleEscape.ReplaceAllString(raw, "")))
}

func clip(line string) string {
	if utf8.RuneCountInString(line) > consoleLineMax {
		return string([]rune(line)[:consoleLineMax]) + "..."
	}
	return line
}

func printable(r rune) rune {
	switch {
	case r == '\t':
		return ' '
	case r == utf8.RuneError || unicode.IsControl(r):
		return -1
	}
	return r
}

func panicReason(line string) string {
	_, reason, ok := strings.Cut(line, "Kernel panic - not syncing:")
	if !ok {
		_, reason, _ = strings.Cut(line, "Kernel panic")
	}
	reason, _, _ = strings.Cut(strings.TrimSpace(reason), "  ")
	if reason = strings.Trim(reason, " -:"); reason == "" {
		return "kernel panic"
	}
	return "kernel panic: " + clip(reason)
}

func resetNote(recoveries []string) string {
	switch len(recoveries) {
	case 0:
		return ""
	case 1:
		return " after 1 automatic reset (" + recoveries[0] + ")"
	}
	return fmt.Sprintf(" after %d automatic resets (%s)", len(recoveries), strings.Join(recoveries, "; "))
}
