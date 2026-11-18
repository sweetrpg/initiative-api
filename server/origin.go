package server

import (
	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/initiative-api/data"
)

// clientIDHeader carries the opaque id a client picks for itself, so it can recognise the echo of
// its own writes on the event stream.
const clientIDHeader = "X-Initiative-Client-Id"

// withOrigin records who is writing, on the request context, for the event each commit emits. A
// client id that is not [A-Za-z0-9._-]{1,64} is ignored, never rejected.
func withOrigin() gin.HandlerFunc {
	return func(c *gin.Context) {
		o := data.Origin{Kind: data.OriginUser, ActorID: authz.Viewer(c)}
		if _, viaCredential := c.Get(credentialContextKey); viaCredential {
			o.Kind = data.OriginIntegration
		}
		if id := c.GetHeader(clientIDHeader); data.ValidClientID(id) {
			o.ClientID = id
		}
		c.Request = c.Request.WithContext(data.WithOrigin(c.Request.Context(), o))
		c.Next()
	}
}
