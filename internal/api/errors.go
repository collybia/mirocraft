// Package api exposes the daemon over HTTP and WebSocket. It is a thin layer:
// every decision about server state lives in the daemon and runner packages,
// per the project rule that management logic exists in exactly one place.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Machine-readable error codes from docs/API.md. Clients switch on these, not
// on the human-readable message.
const (
	CodeValidationFailed = "validation_failed"
	CodeUnauthorized     = "unauthorized"
	CodeForbidden        = "forbidden"
	CodeServerNotFound   = "server_not_found"
	CodeServerNotRunning = "server_not_running"
	CodeRateLimited      = "rate_limited"
	CodeInternalError    = "internal_error"
)

type errorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("writing json response failed", slog.String("error", err.Error()))
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: errorBody{Code: code, Message: message}})
}

func writeErrorDetails(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	writeJSON(w, status, errorResponse{Error: errorBody{Code: code, Message: message, Details: details}})
}
