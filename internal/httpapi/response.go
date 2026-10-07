package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/fl4metf/vm-harness/vm"
)

type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func writeBytes(w http.ResponseWriter, contentType string, data []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func writeError(w http.ResponseWriter, err error) {
	fail(w, statusOf(err), vm.Code(err), err.Error())
}

func fail(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: errorDetail{Code: code, Message: message}})
}

func statusOf(err error) int {
	switch {
	case errors.Is(err, errTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, errMediaType):
		return http.StatusUnsupportedMediaType
	}
	switch vm.Code(err) {
	case vm.CodeNotFound:
		return http.StatusNotFound
	case vm.CodeExists, vm.CodeInvalidState, vm.CodeLimit:
		return http.StatusConflict
	case vm.CodeInvalid:
		return http.StatusBadRequest
	case vm.CodeForbidden:
		return http.StatusForbidden
	case vm.CodeUnsupported:
		return http.StatusNotImplemented
	case vm.CodeNotReady, vm.CodeUnavailable:
		return http.StatusServiceUnavailable
	case vm.CodeTimeout:
		return http.StatusGatewayTimeout
	case vm.CodeCanceled:
		return http.StatusRequestTimeout
	}
	return http.StatusInternalServerError
}
