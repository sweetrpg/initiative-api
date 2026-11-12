package server

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
)

func TestGatedSystemErrorsAreTheTypedEntitlementError(t *testing.T) {
	for _, err := range []error{data.ErrLocked, fmt.Errorf("wrapped: %w", &domain.EntitlementRequiredError{Key: domain.KeyGameSystemsExtra})} {
		body, status := classifyError(err)
		if status != http.StatusForbidden || body.Code != "entitlement_required" || body.Key != domain.KeyGameSystemsExtra {
			t.Errorf("%v = %d %+v", err, status, body)
		}
	}
}
