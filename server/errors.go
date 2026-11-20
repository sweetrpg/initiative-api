package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/entitlements"
	"github.com/sweetrpg/initiative-api/events/stream"
	"github.com/sweetrpg/initiative-api/relations"
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
	if errors.Is(context.Cause(c.Request.Context()), errCredentialRevoked) {
		err = errCredentialRevoked // the request was cut short by the credential's end, whatever the store reported
	}
	body, status := classifyError(err)
	body.CorrelationID = CorrelationID(c)
	if status == http.StatusConflict && body.Code == "revision_conflict" {
		body.Current = current
	}
	if status >= http.StatusInternalServerError && status != http.StatusServiceUnavailable {
		slog.ErrorContext(c.Request.Context(), "request failed", "error", err, "correlation_id", body.CorrelationID)
	}
	if body.Code == "membership_sync_pending" || body.Code == "rate_limited" || body.Code == "timeout" || body.Code == "shutting_down" || body.Code == "too_many_streams" || body.Code == "streams_unavailable" {
		c.Header("Retry-After", "5")
	}
	c.AbortWithStatusJSON(status, body)
}

func classifyError(err error) (ErrorBody, int) {
	var ee *domain.EntitlementError
	var er *domain.EntitlementRequiredError
	var he *data.HealthError
	switch {
	case errors.As(err, &he):
		// Typed validation failure (initiative-api-resources, "Typed error responses"): the code is
		// stable and the key names the offending health field, so a client can mark it.
		if he.Unknown {
			return ErrorBody{Code: "unknown_field", Message: "That health field is not defined by the game system.", Key: he.Field}, http.StatusUnprocessableEntity
		}
		return ErrorBody{Code: "field_out_of_range", Message: "That health value is outside the allowed range for the game system.", Key: he.Field}, http.StatusUnprocessableEntity
	case errors.As(err, &ee):
		return ErrorBody{Code: "entitlement_limit_reached", Message: "Your plan's limit for this item has been reached.", Key: ee.Key, Limit: &ee.Limit}, http.StatusForbidden
	case errors.Is(err, domain.ErrReadOnly):
		return ErrorBody{Code: "packaged_theme_read_only", Message: "Packaged themes cannot be changed. Create a custom theme from it instead."}, http.StatusForbidden
	case errors.Is(err, domain.ErrCredentialsClosed):
		return ErrorBody{Code: "credentials_unavailable", Message: "Integration credentials are temporarily unavailable."}, http.StatusServiceUnavailable
	case errors.Is(err, domain.ErrOwnerLimitsUnknown):
		return ErrorBody{Code: "owner_entitlements_pending", Message: "The session owner has not connected yet. Try again after they do."}, http.StatusServiceUnavailable
	case errors.Is(err, domain.ErrSyncPending):
		return ErrorBody{Code: "membership_sync_pending", Message: "Your change was saved, but access is still being updated. Repeat the request in a moment."}, http.StatusServiceUnavailable
	case errors.Is(err, domain.ErrLimitsNotSynced):
		return ErrorBody{Code: "entitlements_unavailable", Message: "Your plan limits have not been synced yet. Open Initiative once with your own sign-in, then try again."}, http.StatusServiceUnavailable
	case errors.Is(err, relations.ErrDenied):
		return ErrorBody{Code: "forbidden", Message: "You may not do that."}, http.StatusForbidden
	case errors.Is(err, relations.ErrRejected):
		// A malformed request is our bug and retrying cannot help; not 503. It falls to the opaque
		// 500 and writeError logs it.
		return ErrorBody{Code: "internal", Message: "Something went wrong."}, http.StatusInternalServerError
	case errors.Is(err, relations.ErrConflict):
		// The member record is already saved, and the reconciler writes tuples from Mongo through
		// the internal path, which the 200-tuple cap does not cover, so a refused member is let in
		// on the next pass. Accepted: the members_per_session entitlement caps a session at 20
		// members, and only a direct caller of auth-api can reach the tuple cap.
		return ErrorBody{Code: "session_full", Message: "The session has reached its member limit."}, http.StatusConflict
	case errors.Is(err, relations.ErrLimited):
		return ErrorBody{Code: "rate_limited", Message: "Too many requests. Try again shortly."}, http.StatusTooManyRequests
	case errors.Is(err, relations.ErrUnavailable):
		return ErrorBody{Code: "authorization_unavailable", Message: "Session access cannot be checked right now. Try again shortly."}, http.StatusServiceUnavailable
	case errors.Is(err, relations.ErrUnauthorized):
		return ErrorBody{Code: "unauthorized", Message: "Your session is not valid."}, http.StatusUnauthorized
	case errors.Is(err, entitlements.ErrUnavailable):
		return ErrorBody{Code: "entitlements_unavailable", Message: "Entitlements are temporarily unavailable. Try again shortly."}, http.StatusServiceUnavailable
	case errors.Is(err, errCredentialRevoked):
		return ErrorBody{Code: "unauthorized", Message: "This credential has been revoked."}, http.StatusUnauthorized
	case errors.Is(err, entitlements.ErrUnauthorized):
		return ErrorBody{Code: "unauthorized", Message: "Your session is not valid."}, http.StatusUnauthorized
	case errors.Is(err, context.DeadlineExceeded):
		return ErrorBody{Code: "timeout", Message: "The request took too long. Try again shortly."}, http.StatusServiceUnavailable
	case errors.Is(err, data.ErrStreamUnavailable):
		return ErrorBody{Code: "streams_unavailable", Message: "Event streams cannot be opened right now. Try again shortly."}, http.StatusServiceUnavailable
	case errors.Is(err, data.ErrStreamLimit):
		return ErrorBody{Code: "too_many_streams", Message: "You have too many open event streams. Close one and try again."}, http.StatusTooManyRequests
	case errors.Is(err, stream.ErrShuttingDown):
		return ErrorBody{Code: "shutting_down", Message: "This server is restarting. Reconnect shortly."}, http.StatusServiceUnavailable
	case errors.Is(err, domain.ErrTicketInvalid):
		return ErrorBody{Code: "ticket_invalid", Message: "The stream ticket is not valid. Request a new one."}, http.StatusUnauthorized
	case errors.Is(err, domain.ErrChangeStale):
		return ErrorBody{Code: "change_request_stale", Message: strings.TrimPrefix(err.Error(), domain.ErrChangeStale.Error()+": ")}, http.StatusConflict
	case errors.Is(err, data.ErrNotFound):
		return ErrorBody{Code: "not_found", Message: "Not found."}, http.StatusNotFound
	case errors.Is(err, data.ErrLocked), errors.As(err, &er):
		return ErrorBody{Code: "entitlement_required", Message: "This needs an entitlement your account does not currently hold.", Key: domain.KeyGameSystemsExtra}, http.StatusForbidden
	case errors.Is(err, data.ErrInactive):
		return ErrorBody{Code: "item_inactive", Message: "This item is over your plan's limit. You can still view or delete it; choose which items stay active to use it again."}, http.StatusForbidden
	case errors.Is(err, data.ErrNotMember):
		return ErrorBody{Code: "forbidden", Message: "You are not a member of this session."}, http.StatusForbidden
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
