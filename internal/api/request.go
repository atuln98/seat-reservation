package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
)

type validationErrors map[string]string

type validationErrorResponse struct {
	Error  string           `json:"error"`
	Fields validationErrors `json:"fields"`
}

var errUnsupportedMediaType = errors.New("content type must be application/json")

func decodeJSON(writer http.ResponseWriter, request *http.Request, destination any) error {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errUnsupportedMediaType
	}

	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}
