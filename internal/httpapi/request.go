package httpapi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/fl4metf/vm-harness/harness"
	"github.com/fl4metf/vm-harness/internal/strictjson"
	"github.com/fl4metf/vm-harness/vm"
)

const (
	maxJSONBody = 1 << 20
	maxUpload   = 256 << 20
)

var (
	errTooLarge  = errors.New("request body too large")
	errMediaType = errors.New("unsupported media type")
)

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err != nil {
		return bodyError(err)
	}
	if len(data) == 0 {
		return nil
	}
	contentType := r.Header.Get("Content-Type")
	if mediaType, _, _ := mime.ParseMediaType(contentType); mediaType != "application/json" {
		return fmt.Errorf("%w %q, send the body with Content-Type: application/json: %w", errMediaType, contentType, vm.ErrInvalid)
	}
	if err := strictjson.Decode(bytes.NewReader(data), v); err != nil {
		return fmt.Errorf("request body: %w", err)
	}
	return nil
}

func readUpload(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.ContentLength > maxUpload {
		return nil, tooLarge(maxUpload)
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUpload))
	if err != nil {
		return nil, bodyError(err)
	}
	return data, nil
}

func bodyError(err error) error {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return tooLarge(maxErr.Limit)
	}
	return fmt.Errorf("invalid request body: %v: %w", err, vm.ErrInvalid)
}

func tooLarge(limit int64) error {
	return fmt.Errorf("%w, the limit is %d bytes: %w", errTooLarge, limit, vm.ErrLimit)
}

func vmRef(r *http.Request) harness.Ref {
	return harness.Ref{Provider: r.URL.Query().Get("provider"), VM: r.PathValue("vm")}
}

func queryBool(r *http.Request, name string) (bool, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("query parameter %s=%q is not a boolean: %w", name, value, vm.ErrInvalid)
	}
	return b, nil
}

func guestPath(r *http.Request) (string, error) {
	path := r.URL.Query().Get("path")
	if path == "" {
		return "", fmt.Errorf("query parameter path with the guest file path is required: %w", vm.ErrInvalid)
	}
	return path, nil
}

func seconds(n int) (time.Duration, error) {
	if n < 0 {
		return 0, fmt.Errorf("timeout_sec must not be negative: %w", vm.ErrInvalid)
	}
	return time.Duration(n) * time.Second, nil
}

func headerAccess(h http.Header) harness.Access {
	return harness.Access{
		Credentials: vm.Credentials{User: h.Get("X-VMH-User"), Password: h.Get("X-VMH-Password")},
		Transport:   h.Get("X-VMH-Transport"),
		SSH: harness.SSHOptions{
			User:     h.Get("X-VMH-SSH-User"),
			KeyPath:  h.Get("X-VMH-SSH-Key"),
			Password: h.Get("X-VMH-SSH-Password"),
		},
	}
}
