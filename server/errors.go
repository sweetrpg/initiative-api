package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/entitlements"
)

// ErrorBody is the typed error response: a stable code, a safe message, the correlation ID, and
// for entitlement denials the key and limit. Current carries the stored record on a 409.
type ErrorBody struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	CorrelationID string `json:"correlation_id,omitempty"`
	Key           string `json:"key,omitempty"`
	Limit         *int   `json:"limit,omitempty"`
	Current       any    `json:"current,omitempty"`
}

// writeError maps err to a status and typed body. current is returned with revision conflicts.
// Unrecognized errors are logged and reported as an opaque 500.
func writeError(c *gin.Context, err error, current any) {
	body, status := classifyError(err)
	body.CorrelationID = CorrelationID(c)
	if status == http.StatusConflict && body.Code == "revision_conflict" {
		body.Current = current
	}
	if status >= http.StatusInternalServerError && status != http.StatusServiceUnavailable {
		slog.ErrorContext(c.Request.Context(), "request failed", "error", err, "correlation_id", body.CorrelationID)
	}
	c.AbortWithStatusJSON(status, body)
}

func classifyError(err error) (ErrorBody, int) {
	var ee *domain.EntitlementError
	switch {
	case errors.As(err, &ee):
		return ErrorBody{Code: "entitlement_limit_reached", Message: "Your plan's limit for this item has been reached.", Key: ee.Key, Limit: &ee.Limit}, http.StatusForbidden
	case errors.Is(err, domain.ErrReadOnly):
		return ErrorBody{Code: "packaged_theme_read_only", Message: "Packaged themes cannot be changed. Create a custom theme from it instead."}, http.StatusForbidden
	case errors.Is(err, domain.ErrCredentialsClosed):
		return ErrorBody{Code: "credentials_unavailable", Message: "Integration credentials are temporarily unavailable."}, http.StatusServiceUnavailable
	case errors.Is(err, entitlements.ErrUnavailable):
		return ErrorBody{Code: "entitlements_unavailable", Message: "Entitlements are temporarily unavailable. Try again shortly."}, http.StatusServiceUnavailable
	case errors.Is(err, entitlements.ErrUnauthorized):
		return ErrorBody{Code: "unauthorized", Message: "Your session is not valid."}, http.StatusUnauthorized
	case errors.Is(err, data.ErrNotFound):
		return ErrorBody{Code: "not_found", Message: "Not found."}, http.StatusNotFound
	case errors.Is(err, data.ErrForbidden):
		return ErrorBody{Code: "forbidden", Message: "You do not own this record."}, http.StatusForbidden
	case errors.Is(err, data.ErrRevisionConflict):
		return ErrorBody{Code: "revision_conflict", Message: "The record changed since you loaded it."}, http.StatusConflict
	case errors.Is(err, data.ErrConflict):
		return ErrorBody{Code: "conflict", Message: "The request conflicts with an existing record."}, http.StatusConflict
	case errors.Is(err, data.ErrInvalid):
		return ErrorBody{Code: "invalid", Message: strings.TrimPrefix(err.Error(), data.ErrInvalid.Error()+": ")}, http.StatusUnprocessableEntity
	}
	return ErrorBody{Code: "internal", Message: "Something went wrong."}, http.StatusInternalServerError
}

// badRequest reports a malformed request body or parameter.
func badRequest(c *gin.Context, msg string) {
	c.AbortWithStatusJSON(http.StatusBadRequest, ErrorBody{Code: "bad_request", Message: msg, CorrelationID: CorrelationID(c)})
}
