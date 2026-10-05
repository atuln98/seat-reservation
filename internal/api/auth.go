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
			writeError(writer, http.StatusUnsupportedMediaType, "content_type_must_be_application_json")
		} else {
			writeError(writer, http.StatusBadRequest, "invalid_request")
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
		if writeContextError(writer, err) {
			return
		}
		switch {
		case errors.Is(err, user.ErrEmailExists):
			writeError(writer, http.StatusConflict, "email_already_registered")
		case errors.Is(err, user.ErrInvalidEmail):
			writeError(writer, http.StatusBadRequest, "invalid_email")
		case errors.Is(err, user.ErrInvalidPassword):
			writeError(writer, http.StatusBadRequest, "invalid_password")
		default:
			requestLogger(api.logger, request).Error(
				"user registration failed",
				"event", "user_registration_failed",
				"error", err,
			)
			writeError(writer, http.StatusInternalServerError, "internal_error")
		}
		return
	}

	requestLogger(api.logger, request).Info(
		"user registered",
		"event", "user_registered",
		"userId", authentication.ID,
		"role", authentication.Role,
	)
	writeJSON(writer, http.StatusCreated, authentication)
}

func (api *API) login(writer http.ResponseWriter, request *http.Request) {
	credentials, ok := readCredentials(writer, request)
	if !ok {
		return
	}

	authentication, err := api.users.Login(request.Context(), credentials.Email, credentials.Password)
	if err != nil {
		if writeContextError(writer, err) {
			return
		}
		if errors.Is(err, user.ErrInvalidCredentials) {
			writeError(writer, http.StatusUnauthorized, "invalid_credentials")
			return
		}
		requestLogger(api.logger, request).Error(
			"user login failed",
			"event", "user_login_failed",
			"error", err,
		)
		writeError(writer, http.StatusInternalServerError, "internal_error")
		return
	}

	requestLogger(api.logger, request).Info(
		"user authenticated",
		"event", "user_authenticated",
		"userId", authentication.ID,
		"role", authentication.Role,
	)
	writeJSON(writer, http.StatusOK, authentication)
}

func (api *API) currentUser(writer http.ResponseWriter, request *http.Request) {
	principal, ok := auth.PrincipalFromContext(request.Context())
	if !ok {
		writeError(writer, http.StatusUnauthorized, "authentication_required")
		return
	}

	existing, err := api.users.Get(request.Context(), principal.UserID)
	if err != nil {
		if writeContextError(writer, err) {
			return
		}
		if errors.Is(err, user.ErrNotFound) {
			writeError(writer, http.StatusUnauthorized, "invalid_token")
			return
		}
		requestLogger(api.logger, request).Error(
			"user lookup failed",
			"event", "user_lookup_failed",
			"userId", principal.UserID,
			"error", err,
		)
		writeError(writer, http.StatusInternalServerError, "internal_error")
		return
	}

	writeJSON(writer, http.StatusOK, existing)
}
