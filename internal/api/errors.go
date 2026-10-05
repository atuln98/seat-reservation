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
		writeJSON(writer, statusClientClosedRequest, map[string]string{"error": "request_cancelled"})
		return true
	case errors.Is(err, context.DeadlineExceeded):
		writeJSON(writer, http.StatusRequestTimeout, map[string]string{"error": "request_timeout"})
		return true
	default:
		return false
	}
}
