package vm

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrExists       = errors.New("already exists")
	ErrInvalidState = errors.New("invalid state")
	ErrInvalid      = errors.New("invalid argument")
	ErrUnsupported  = errors.New("unsupported")
	ErrNotReady     = errors.New("not ready")
	ErrUnavailable  = errors.New("provider unavailable")
	ErrForbidden    = errors.New("forbidden")
	ErrLimit        = errors.New("limit exceeded")
)

type CommandError struct {
	Path     string
	Args     []string
	ExitCode int
	Stdout   string
	Stderr   string
	Kind     error
}

func (e *CommandError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(e.Stdout)
	}
	if len(msg) > 2000 {
		msg = msg[:2000] + "..."
	}
	cmd := e.Path
	if len(e.Args) > 0 {
		cmd += " " + e.Args[0]
	}
	if e.Kind != nil {
		return fmt.Sprintf("%s: %s (exit %d): %s", cmd, e.Kind, e.ExitCode, msg)
	}
	return fmt.Sprintf("%s: exit %d: %s", cmd, e.ExitCode, msg)
}

func (e *CommandError) Unwrap() error { return e.Kind }

const (
	CodeNotFound     = "not_found"
	CodeExists       = "already_exists"
	CodeInvalidState = "invalid_state"
	CodeInvalid      = "invalid_argument"
	CodeUnsupported  = "unsupported"
	CodeNotReady     = "not_ready"
	CodeUnavailable  = "unavailable"
	CodeForbidden    = "forbidden"
	CodeLimit        = "limit_exceeded"
	CodeTimeout      = "timeout"
	CodeCanceled     = "canceled"
	CodeInternal     = "internal"
)

var codes = []struct {
	err  error
	code string
}{
	{ErrNotFound, CodeNotFound},
	{ErrExists, CodeExists},
	{ErrInvalidState, CodeInvalidState},
	{ErrInvalid, CodeInvalid},
	{ErrUnsupported, CodeUnsupported},
	{ErrNotReady, CodeNotReady},
	{ErrUnavailable, CodeUnavailable},
	{ErrForbidden, CodeForbidden},
	{ErrLimit, CodeLimit},
	{context.DeadlineExceeded, CodeTimeout},
	{context.Canceled, CodeCanceled},
}

func Code(err error) string {
	if err == nil {
		return ""
	}
	for _, c := range codes {
		if errors.Is(err, c.err) {
			return c.code
		}
	}
	return CodeInternal
}
