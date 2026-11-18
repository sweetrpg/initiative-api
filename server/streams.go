package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/sweetrpg/authz-client.go/authz"
	"github.com/sweetrpg/initiative-api/constants"
	"github.com/sweetrpg/initiative-api/data"
	"github.com/sweetrpg/initiative-api/domain"
	"github.com/sweetrpg/initiative-api/events/stream"
)

// WebSocket close codes the service sends (application range 4000-4999).
const (
	CloseCredentialRevoked websocket.StatusCode = 4001 // the credential that opened the stream is gone
	CloseMembershipChanged websocket.StatusCode = 4002 // access changed or ended; reconnect to find out
	CloseSlowConsumer      websocket.StatusCode = 4003 // the client fell behind; reconnect and refetch
	CloseMaxAge            websocket.StatusCode = 4004 // the connection reached its maximum age
	streamBuffer                                = 64   // events buffered per connection
	writeTimeout                                = 10 * time.Second
	pongTimeout                                 = 60 * time.Second
	ticketQuery                                 = "ticket"
	ticketKey                                   = "initiative.stream_ticket"
)

// StreamOptions are the transport settings of the event stream.
type StreamOptions struct {
	// AllowedOrigins are host patterns (path.Match) a browser's Origin may match; no Origin header
	// is always allowed.
	AllowedOrigins []string
	// MaxAge ends a stream after this long (default 1h); Heartbeat is the interval of the
	// application heartbeat, the protocol ping, and the membership re-check (default 20s).
	MaxAge, Heartbeat time.Duration
}

func (o StreamOptions) maxAge() time.Duration {
	if o.MaxAge > 0 {
		return o.MaxAge
	}
	return time.Hour
}

func (o StreamOptions) heartbeat() time.Duration {
	if o.Heartbeat > 0 {
		return o.Heartbeat
	}
	return 20 * time.Second
}

// RedactTicket takes the stream ticket out of the request URL before anything logs it, and keeps
// it for the stream handler. The ticket is single-use and short-lived, so a leaked one is mostly
// harmless, but it should never reach an access log.
func RedactTicket() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.RawQuery != "" {
			q := c.Request.URL.Query()
			if t := q.Get(ticketQuery); t != "" {
				c.Set(ticketKey, t)
				q.Del(ticketQuery)
				c.Request.URL.RawQuery = q.Encode()
			}
		}
		c.Next()
	}
}

type streamHandlers struct {
	svc  Services
	opts StreamOptions
}

// setupStreamHandlers registers POST /streams/tickets (user bearer only) on u, and GET /streams on
// r: with ticket= it opens the stream for that ticket, with no ticket it needs the caller's bearer
// (a user's, or an integration credential with the read scope) like every other route.
func setupStreamHandlers(r *gin.Engine, u *gin.RouterGroup, s Services) {
	h := streamHandlers{s, s.Stream}
	u.POST("/streams/tickets", h.mint)
	g := r.Group("", h.ticketGate, typedErrors(), authenticate(s.Credentials, authz.RequireAnyRole(s.Authz, constants.ServiceName, authz.RoleUser)))
	g.Use(withOrigin(), withGate(s.Relations))
	g.GET("/streams", h.bearerStream)
}

type ticketBody struct {
	SessionID   string `json:"session_id"`
	EncounterID string `json:"encounter_id"`
	ClientID    string `json:"client_id"`
}

func (h streamHandlers) mint(c *gin.Context) {
	var b ticketBody
	if !bindJSON(c, &b) {
		return
	}
	ticket, ttl, err := h.svc.Streams.Mint(c.Request.Context(), authz.Viewer(c), domain.TicketRequest{SessionID: b.SessionID, EncounterID: b.EncounterID, ClientID: b.ClientID})
	if err != nil {
		writeError(c, err, nil)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, gin.H{"ticket": ticket, "expires_in": int(ttl.Seconds())})
}

// originAllowed applies the same rule the upgrade does, before a ticket is spent on it.
func (o StreamOptions) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	for _, p := range o.AllowedOrigins {
		if ok, _ := path.Match(strings.ToLower(p), strings.ToLower(u.Host)); ok {
			return true
		}
	}
	return false
}

// ticketGate serves a request that carries a ticket; any other request continues to the bearer
// chain. The ticket stands in for a bearer the browser cannot send on an upgrade.
func (h streamHandlers) ticketGate(c *gin.Context) {
	ticket := c.GetString(ticketKey)
	if ticket == "" {
		ticket = c.Query(ticketQuery) // RedactTicket not installed
	}
	if ticket == "" {
		c.Next()
		return
	}
	c.Abort()
	if !h.opts.originAllowed(c.Request) {
		c.JSON(http.StatusForbidden, ErrorBody{Code: "origin_not_allowed", Message: "This origin may not open a stream.", CorrelationID: CorrelationID(c)})
		return
	}
	claim, err := h.svc.Streams.Redeem(c.Request.Context(), ticket)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	h.serve(c, serveArgs{
		actor: claim.UserID, session: claim.SessionID, encounter: claim.EncounterID, clientID: claim.ClientID,
		authorize: func(ctx context.Context) (data.Viewer, error) { return h.svc.Streams.View(ctx, claim) },
	})
}

// bearerStream opens the stream for a caller authenticated by bearer: authorized exactly as the
// REST reads are (OpenFGA through the caller's own token, fail closed).
func (h streamHandlers) bearerStream(c *gin.Context) {
	if !h.opts.originAllowed(c.Request) {
		c.JSON(http.StatusForbidden, ErrorBody{Code: "origin_not_allowed", Message: "This origin may not open a stream.", CorrelationID: CorrelationID(c)})
		return
	}
	actor, session, enc := authz.Viewer(c), c.Query("session_id"), c.Query("encounter_id")
	if _, viaCredential := c.Get(credentialContextKey); viaCredential && session != "" {
		// Session data is authorized with the caller's own bearer (PADR-0037); a credential has none.
		userOnly(c)
		return
	}
	client := c.Query("client_id")
	if client == "" {
		client = c.GetHeader(clientIDHeader)
	}
	a := serveArgs{actor: actor, session: session, encounter: enc, clientID: client}
	a.authorize = func(ctx context.Context) (data.Viewer, error) {
		return h.svc.Streams.BearerView(ctx, actor, session, enc)
	}
	if id, ok := c.Get(credentialContextKey); ok {
		a.guard = func(ctx context.Context) error {
			if live, err := h.svc.Credentials.Live(ctx, id.(string)); err == nil && !live {
				return errCredentialRevoked
			}
			return nil
		}
	}
	h.serve(c, a)
}

var errCredentialRevoked = errors.New("credential revoked")

type serveArgs struct {
	actor, session, encounter, clientID string
	authorize                           func(context.Context) (data.Viewer, error)
	// guard runs on every heartbeat, besides the membership re-check; an error ends the stream.
	guard func(context.Context) error
}

// serve opens a pending subscription, authorizes, grants, and upgrades. Everything that can fail
// does so before the upgrade, as an ordinary HTTP error.
func (h streamHandlers) serve(c *gin.Context, a serveArgs) {
	ctx := c.Request.Context()
	sub := h.svc.Hub.Open(a.session, a.actor, a.encounter, streamBuffer)
	defer sub.Close()
	v, err := a.authorize(ctx)
	if err == nil {
		err = sub.Grant(v)
		if errors.Is(err, stream.ErrEvicted) || errors.Is(err, stream.ErrViewer) {
			err = data.ErrNotMember
		}
	}
	if err != nil {
		writeError(c, err, nil)
		return
	}
	heads, err := h.svc.Streams.Heads(ctx, v, a.encounter)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	conn, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{OriginPatterns: h.opts.AllowedOrigins})
	if err != nil {
		return // Accept has answered
	}
	defer func() { _ = conn.CloseNow() }()
	cl := &connection{h: h, conn: conn, sub: sub, v: v, a: a}
	if err := cl.hello(ctx, heads); err != nil {
		return
	}
	cl.run(ctx)
}

type headFrame struct {
	Type string `json:"type"`
	domain.Head
}

type connection struct {
	h    streamHandlers
	conn *websocket.Conn
	sub  *stream.Subscription
	v    data.Viewer
	a    serveArgs
}

func (cl *connection) write(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return cl.conn.Write(ctx, websocket.MessageText, b)
}

// hello sends the hello frame and one head frame per encounter in scope. They carry only what the
// viewer's own views carry: the scope they asked for and each visible encounter's revision (GM) or
// view tag.
func (cl *connection) hello(ctx context.Context, heads []domain.Head) error {
	opts := cl.h.opts
	if err := cl.write(ctx, gin.H{
		"type": "hello", "session_id": cl.a.session, "encounter_id": cl.a.encounter, "client_id": cl.a.clientID,
		"heartbeat_seconds": int(opts.heartbeat().Seconds()), "max_age_seconds": int(opts.maxAge().Seconds()),
	}); err != nil {
		return err
	}
	for _, hd := range heads {
		if err := cl.write(ctx, headFrame{"head", hd}); err != nil {
			return err
		}
	}
	return nil
}

// run delivers events until the stream ends for any reason, and closes with the code that says
// why. Client frames are read and discarded (CloseRead), so pongs reset the idle watch.
func (cl *connection) run(ctx context.Context) {
	ctx = cl.conn.CloseRead(ctx)
	tick := time.NewTicker(cl.h.opts.heartbeat())
	defer tick.Stop()
	maxAge := time.NewTimer(cl.h.opts.maxAge())
	defer maxAge.Stop()
	pings := make(chan struct{}, 1)
	go cl.pinger(ctx, pings)
	for {
		select {
		case ev, ok := <-cl.sub.C:
			if !ok {
				if cl.sub.Overflowed() {
					_ = cl.conn.Close(CloseSlowConsumer, "slow consumer")
				} else {
					_ = cl.conn.Close(CloseMembershipChanged, "membership changed")
				}
				return
			}
			if cl.write(ctx, ev) != nil {
				return
			}
		case <-tick.C:
			if cl.write(ctx, gin.H{"type": "heartbeat"}) != nil {
				return
			}
			select {
			case pings <- struct{}{}:
			default:
			}
			if code, why := cl.check(ctx); code != 0 {
				_ = cl.conn.Close(code, why)
				return
			}
		case <-maxAge.C:
			_ = cl.conn.Close(CloseMaxAge, "maximum connection age")
			return
		case <-ctx.Done():
			return
		}
	}
}

// check re-reads what the stream was authorized on. A failed read keeps the stream (it is
// fail-open here: the bound on a missed eviction is the maximum age, and the next check retries).
func (cl *connection) check(ctx context.Context) (websocket.StatusCode, string) {
	if cl.a.guard != nil {
		if err := cl.a.guard(ctx); errors.Is(err, errCredentialRevoked) {
			return CloseCredentialRevoked, "credential revoked"
		}
	}
	if errors.Is(cl.h.svc.Streams.Recheck(ctx, cl.v), domain.ErrAccessChanged) {
		return CloseMembershipChanged, "membership changed"
	}
	return 0, ""
}

// pinger sends a protocol ping per request and ends the connection when no pong comes back.
func (cl *connection) pinger(ctx context.Context, pings <-chan struct{}) {
	for {
		select {
		case <-pings:
			pctx, cancel := context.WithTimeout(ctx, pongTimeout)
			err := cl.conn.Ping(pctx)
			cancel()
			if err != nil && ctx.Err() == nil {
				_ = cl.conn.Close(websocket.StatusGoingAway, "idle")
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
