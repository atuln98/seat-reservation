package api

import (
	"context"
	"errors"
	"net/http"
)

const statusClientClosedRequest = 499

func writeContextError(writer http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, context.Canceled):
		writeError(writer, statusClientClosedRequest, "request_cancelled")
		return true
	case errors.Is(err, context.DeadlineExceeded):
		writeError(writer, http.StatusRequestTimeout, "request_timeout")
		return true
	default:
		return false
	}
}

func writeError(writer http.ResponseWriter, status int, code string) {
	writeJSON(writer, status, map[string]string{"error": code})
}
