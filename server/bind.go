package server

import (
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"

	"github.com/gin-gonic/gin"
)

// maxBodyBytes bounds a request body on the resource routes: 256 KiB, far above a roster or an
// encounter with the most combatants and conditions the data layer allows, and far below what a
// hostile client could use to pin memory.
const maxBodyBytes = 256 << 10

// limitBody caps the request body so a handler never decodes more than maxBodyBytes.
func limitBody() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
		c.Next()
	}
}

var errTrailingData = errors.New("trailing data after the JSON value")

var unknownField = regexp.MustCompile(`unknown field "([^"]*)"`)

// bindJSON decodes the body into v and refuses a field v does not declare. It writes the typed
// error and returns false: 413 payload_too_large past the cap, 422 unknown_body_field naming the
// field, 400 for anything else that is not the JSON the route takes.
func bindJSON(c *gin.Context, v any) bool {
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		// One JSON value only: trailing data is a malformed request. Reading it can still hit the cap.
		_, extra := dec.Token()
		if errors.Is(extra, io.EOF) {
			return true
		}
		err = cmp.Or(extra, errTrailingData)
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, ErrorBody{Code: "payload_too_large", Message: "The request body is too large.", CorrelationID: CorrelationID(c)})
		return false
	}
	if m := unknownField.FindStringSubmatch(err.Error()); m != nil {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, ErrorBody{Code: "unknown_body_field", Message: "The request body has a field this route does not accept.", Key: m[1], CorrelationID: CorrelationID(c)})
		return false
	}
	badRequest(c, "request body must be JSON in the shape this route takes")
	return false
}

// checkFields validates a projection list against the names a resource may project. The names are
// the resource's data.Spec allow-list plus any derived keys; anything else is a 422 like every
// other rejected list parameter.
func checkFields(c *gin.Context, fields, allowed []string) bool {
	for _, f := range fields {
		if !slices.Contains(allowed, f) {
			c.AbortWithStatusJSON(http.StatusUnprocessableEntity, ErrorBody{Code: "invalid", Message: "cannot project " + f, Key: f, CorrelationID: CorrelationID(c)})
			return false
		}
	}
	return true
}

// projectMap keeps the named keys of a rendered resource, plus always (the identity keys a
// client needs to address what it received). Projecting the rendered output rather than the stored
// document means an unselected field is absent, never a zero value that reads as real data.
func projectMap(m map[string]any, fields []string, always ...string) map[string]any {
	if len(fields) == 0 {
		return m
	}
	out := make(map[string]any, len(fields)+len(always))
	for _, k := range append(slices.Clone(fields), always...) {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}

// toMap renders v through its JSON form, for projecting a stored resource.
func toMap(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// fieldsOf reads and validates ?fields= for a route that takes no other list parameters.
func fieldsOf(c *gin.Context, allowed []string) ([]string, bool) {
	fields := splitList(c.Query("fields"))
	return fields, checkFields(c, fields, allowed)
}
