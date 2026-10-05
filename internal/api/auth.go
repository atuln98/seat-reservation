package api

import (
	"errors"
	"net/http"

	"seat-reservation/internal/auth"
	"seat-reservation/internal/user"
)

type credentialsRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (credentials *credentialsRequest) validate() validationErrors {
	fields := validationErrors{}
	if _, err := user.NormalizeEmail(credentials.Email); err != nil {
		fields["email"] = "must be a valid email address"
	}
	if err := user.ValidatePassword(credentials.Password); err != nil {
		fields["password"] = "must contain between 8 and 72 bytes"
	}
	return fields
}

func readCredentials(writer http.ResponseWriter, request *http.Request) (credentialsRequest, bool) {
	var credentials credentialsRequest
	if err := decodeJSON(writer, request, &credentials); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			writeJSON(writer, http.StatusUnsupportedMediaType, map[string]string{"error": "content_type_must_be_application_json"})
		} else {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		}
		return credentialsRequest{}, false
	}

	fields := credentials.validate()
	if len(fields) != 0 {
		writeJSON(writer, http.StatusBadRequest, validationErrorResponse{
			Error:  "validation_failed",
			Fields: fields,
		})
		return credentialsRequest{}, false
	}

	return credentials, true
}

func (api *API) register(writer http.ResponseWriter, request *http.Request) {
	credentials, ok := readCredentials(writer, request)
	if !ok {
		return
	}

	authentication, err := api.users.Register(request.Context(), credentials.Email, credentials.Password)
	if err != nil {
		switch {
		case errors.Is(err, user.ErrEmailExists):
			writeJSON(writer, http.StatusConflict, map[string]string{"error": "email_already_registered"})
		case errors.Is(err, user.ErrInvalidEmail):
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_email"})
		case errors.Is(err, user.ErrInvalidPassword):
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_password"})
		default:
			api.logger.Error("user registration failed", "error", err)
			writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		}
		return
	}

	writeJSON(writer, http.StatusCreated, authentication)
}

func (api *API) login(writer http.ResponseWriter, request *http.Request) {
	credentials, ok := readCredentials(writer, request)
	if !ok {
		return
	}

	authentication, err := api.users.Login(request.Context(), credentials.Email, credentials.Password)
	if err != nil {
		if errors.Is(err, user.ErrInvalidCredentials) {
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "invalid_credentials"})
			return
		}
		api.logger.Error("user login failed", "error", err)
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}

	writeJSON(writer, http.StatusOK, authentication)
}

func (api *API) currentUser(writer http.ResponseWriter, request *http.Request) {
	principal, ok := auth.PrincipalFromContext(request.Context())
	if !ok {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "authentication_required"})
		return
	}

	existing, err := api.users.Get(request.Context(), principal.UserID)
	if err != nil {
		if errors.Is(err, user.ErrNotFound) {
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
			return
		}
		api.logger.Error("user lookup failed", "user_id", principal.UserID, "error", err)
		writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}

	writeJSON(writer, http.StatusOK, existing)
}
