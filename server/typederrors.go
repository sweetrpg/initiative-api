package server

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
)

// typedErrors rewrites the {"error", "message"} bodies that shared middleware writes (authz-client.go
// for 401/403/503, api-core's rate limiter for 429/503) into this API's typed {"code", "message",
// "correlation_id"} shape, so a client reads one error format. Bodies that already have a code pass
// through, and the library's error string becomes the code (the token one, invalid_token, is
// unauthorized like the credential 401). It wraps the writer once per request.
func typedErrors() gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, done := c.Writer.(*typedWriter); !done {
			c.Writer = &typedWriter{ResponseWriter: c.Writer, c: c}
		}
		c.Next()
	}
}

type typedWriter struct {
	gin.ResponseWriter
	c *gin.Context
}

func (w *typedWriter) Write(b []byte) (int, error) {
	switch w.Status() {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable:
		var old struct {
			Error   string  `json:"error"`
			Message string  `json:"message"`
			Code    *string `json:"code"`
		}
		if json.Unmarshal(bytes.TrimSpace(b), &old) == nil && old.Error != "" && old.Code == nil {
			code := old.Error
			if code == "invalid_token" {
				code = "unauthorized"
			}
			out, err := json.Marshal(ErrorBody{Code: code, Message: old.Message, CorrelationID: CorrelationID(w.c)})
			if err != nil {
				return 0, err
			}
			// The rewritten body has a different length than the one the caller sized.
			w.Header().Del("Content-Length")
			if _, err := w.ResponseWriter.Write(out); err != nil {
				return 0, err
			}
			return len(b), nil
		}
	}
	return w.ResponseWriter.Write(b)
}
