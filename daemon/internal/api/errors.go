package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// apiError is the uniform non-2xx body from API.md.
type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Error codes (API.md).
const (
	codeUnauthorized   = "unauthorized"
	codeForbidden      = "forbidden"
	codeNotFound       = "not_found"
	codeInvalidRequest = "invalid_request"
	codeConflict       = "conflict"
	codeRateLimited    = "rate_limited"
	codeInternal       = "internal"
)

func statusFor(code string) int {
	switch code {
	case codeUnauthorized:
		return http.StatusUnauthorized
	case codeForbidden:
		return http.StatusForbidden
	case codeNotFound:
		return http.StatusNotFound
	case codeInvalidRequest:
		return http.StatusBadRequest
	case codeConflict:
		return http.StatusConflict
	case codeRateLimited:
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

func writeError(w http.ResponseWriter, code, msg string) {
	var e apiError
	e.Error.Code = code
	e.Error.Message = msg
	writeJSON(w, statusFor(code), e)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		slog.Error("marshal response", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(buf)
}

// decodeJSON reads a JSON body into dst, returning false (and writing an error)
// if it is malformed.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, codeInvalidRequest, "malformed JSON body: "+err.Error())
		return false
	}
	return true
}
