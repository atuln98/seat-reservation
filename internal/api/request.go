package api

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5/pgtype"
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

func canonicalUUID(value string) (string, bool) {
	var parsed pgtype.UUID
	if parsed.Scan(value) != nil || !parsed.Valid {
		return "", false
	}
	encoded := hex.EncodeToString(parsed.Bytes[:])
	return encoded[:8] + "-" +
		encoded[8:12] + "-" +
		encoded[12:16] + "-" +
		encoded[16:20] + "-" +
		encoded[20:], true
}

func normalizeSeatNumbers(values []string) ([]string, string) {
	if len(values) == 0 {
		return values, "must contain at least one seat"
	}

	normalized := make([]string, len(values))
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		number := strings.TrimSpace(value)
		normalized[index] = number
		if length := len([]byte(number)); length == 0 || length > 32 {
			return normalized, "each seat number must contain between 1 and 32 bytes"
		}
		if containsControlCharacter(number) {
			return normalized, "seat numbers must not contain control characters"
		}
		if _, exists := seen[number]; exists {
			return normalized, "must contain unique seat numbers"
		}
		seen[number] = struct{}{}
	}
	sort.Strings(normalized)
	return normalized, ""
}

func containsControlCharacter(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}
