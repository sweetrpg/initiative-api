// Package server wires the HTTP routes and middleware.
package server

import "github.com/gin-gonic/gin"

// SetupHandlers registers every route group on r.
func SetupHandlers(r *gin.Engine) {
	setupStatusHandlers(r)
}
