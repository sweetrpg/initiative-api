package server

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync/atomic"
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
	maxGuardErrors                              = 3
	checkTimeout                                = 5 * time.Second
	readLimit                                   = 1024
	streamBuffer                                = 64 // events buffered per connection
	writeTimeout                                = 10 * time.Second
	pongTimeout                                 = 60 * time.Second
	ticketQuery                                 = "ticket"
	ticketKey                                   = "initiative.stream_ticket"
	ticketPresentKey                            = "initiative.stream_ticket_present"
	redactedKey                                 = "initiative.stream_redacted"
)

// StreamOptions are the transport settings of the event stream.
type StreamOptions struct {
	// AllowedOrigins are host patterns (path.Match) a browser's Origin may match; no Origin header
	// is always allowed.
	AllowedOrigins []string
	// MaxAge ends a stream after this long (default 1h); Heartbeat is the interval of the
	// application heartbeat, the protocol ping, and the membership re-check (default 20s).
	MaxAge, Heartbeat time.Duration
	// Buffer is how many events a stream may fall behind before it is closed (default 64).
	Buffer int
	// beforeWrite, when set by a test, runs before each event is written.
	beforeWrite func()
	// onServe, when set by a test, runs as serve starts, holding a pod slot.
	onServe func()
	// afterRenew, when set by a test, runs after each seat renewal attempt.
	afterRenew func()
	// ConcurrentMax is the most streams one user may hold (Services.Leases counts them, with
	// seats lasting LeaseTTL unless renewed and kept Grace after a drop); MaxPerPod is this pod's
	// cap, answered with close code 1013. Zero disables the cap.
	ConcurrentMax, CredentialMax, MaxPerPod int
	LeaseTTL, Grace                         time.Duration
	// Pod names this pod on its seats.
	Pod string
}

func (o StreamOptions) buffer() int {
	if o.Buffer > 0 {
		return o.Buffer
	}
	return streamBuffer
}

func (o StreamOptions) leaseTTL() time.Duration {
	if o.LeaseTTL > 0 {
		return o.LeaseTTL
	}
	return constants.DefaultStreamLeaseTTL
}

func (o StreamOptions) maxAge() time.Duration {
	if o.MaxAge > 0 {
		return o.MaxAge
	}
	return constants.DefaultStreamMaxAge
}

func (o StreamOptions) heartbeat() time.Duration {
	if o.Heartbeat > 0 {
		return o.Heartbeat
	}
	return constants.DefaultStreamHeartbeat
}

// RedactTicket takes the stream ticket out of the request URL before anything logs it, and keeps
// it for the stream handler. The ticket is single-use and short-lived, so a leaked one is mostly
// harmless, but it should never reach an access log.
func RedactTicket() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(redactedKey, true)
		if c.Request.URL.RawQuery != "" {
			q := c.Request.URL.Query()
			if q.Has(ticketQuery) {
				for _, t := range q[ticketQuery] { // the first non-empty value is the ticket
					if t != "" {
						c.Set(ticketKey, t)
						break
					}
				}
				c.Set(ticketPresentKey, true)
				q.Del(ticketQuery)
				c.Request.URL.RawQuery = q.Encode()
				c.Request.RequestURI = c.Request.URL.RequestURI() // Recovery and tracing read this one
			}
		}
		c.Next()
	}
}

type streamHandlers struct {
	svc  Services
	opts StreamOptions
	open *atomic.Int64 // connections on this pod
	// refusing bounds the refusals in flight (each holds a socket for the close handshake); nil is
	// unbounded.
	refusing chan struct{}
}

// maxRefusals is how many "pod full" closes may be in flight at once.
const maxRefusals = 32

// setupStreamHandlers registers POST /streams/tickets (user bearer only) on u, and GET /streams on
// r: with ticket= it opens the stream for that ticket, with no ticket it needs the caller's bearer
// (a user's, or an integration credential with the read scope) like every other route.
func setupStreamHandlers(r *gin.Engine, u *gin.RouterGroup, s Services) {
	h := streamHandlers{svc: s, opts: s.Stream, open: new(atomic.Int64), refusing: make(chan struct{}, maxRefusals)}
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
	if _, ok := c.Get(redactedKey); !ok && c.Request.URL.Query().Has(ticketQuery) {
		// RedactTicket was not installed ahead of the access log: refuse rather than serve a
		// request whose ticket may already be in a log. Wiring bug, not a client error.
		slog.ErrorContext(c.Request.Context(), "a stream ticket arrived but RedactTicket is not installed; refusing")
		c.AbortWithStatusJSON(http.StatusInternalServerError, ErrorBody{Code: "internal", Message: "Something went wrong.", CorrelationID: CorrelationID(c)})
		return
	}
	if present, _ := c.Get(ticketPresentKey); present != true {
		c.Next()
		return
	}
	ticket := c.GetString(ticketKey)
	if ticket == "" { // ?ticket= with no value
		c.Abort()
		writeError(c, domain.ErrTicketInvalid, nil)
		return
	}
	c.Abort()
	if !h.opts.originAllowed(c.Request) {
		c.JSON(http.StatusForbidden, ErrorBody{Code: "origin_not_allowed", Message: "This origin may not open a stream.", CorrelationID: CorrelationID(c)})
		return
	}
	ctx := c.Request.Context()
	// Peek reads the ticket without spending it, so a junk ticket is a plain 401 (no upgrade, no
	// socket held) and a refused connect leaves a real one redeemable.
	claim, err := h.svc.Streams.Peek(ctx, ticket)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	release, ok := h.admit()
	if !ok {
		h.refuseFull(c)
		return
	}
	defer release()
	a := serveArgs{
		actor: claim.UserID, session: claim.SessionID, encounter: claim.EncounterID, clientID: claim.ClientID,
		authorize: func(ctx context.Context) (data.Viewer, error) { return h.svc.Streams.View(ctx, claim) },
		redeem:    func(ctx context.Context) error { _, err := h.svc.Streams.Redeem(ctx, ticket); return err },
	}
	h.serve(c, a)
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
	if !data.ValidClientID(client) {
		client = "" // ignored, never rejected, like the origin of a write
	}
	a := serveArgs{actor: actor, session: session, encounter: enc, clientID: client}
	a.authorize = func(ctx context.Context) (data.Viewer, error) {
		return h.svc.Streams.BearerView(ctx, actor, session, enc)
	}
	if id, ok := c.Get(credentialContextKey); ok {
		a.guard = liveGuard(func(ctx context.Context) (bool, error) {
			return h.svc.Credentials.Live(ctx, id.(string))
		})
	}
	if id, ok := c.Get(credentialContextKey); ok {
		a.credentialID = id.(string)
	}
	release, ok := h.admit()
	if !ok {
		h.refuseFull(c)
		return
	}
	defer release()
	h.serve(c, a)
}

var errCredentialRevoked = errors.New("credential revoked")

// liveGuard turns a liveness read into a heartbeat guard. A revoked credential ends the stream at
// once. A store error keeps it until maxGuardErrors checks in a row have failed, then closes it as
// revoked: fail closed, but not on one blip, so a missed revocation is bounded by that many
// heartbeats. A successful read resets the count.
func liveGuard(live func(context.Context) (bool, error)) func(context.Context) error {
	var failed int
	return func(ctx context.Context) error {
		ok, err := live(ctx)
		switch {
		case err != nil:
			if failed++; failed >= maxGuardErrors {
				return errCredentialRevoked
			}
		case !ok:
			return errCredentialRevoked
		default:
			failed = 0
		}
		return nil
	}
}

type serveArgs struct {
	actor, session, encounter, clientID string
	authorize                           func(context.Context) (data.Viewer, error)
	// guard runs on every heartbeat, besides the membership re-check; an error ends the stream.
	guard func(context.Context) error
	// credentialID is set when an integration credential opened the stream; its seats count in
	// their own bucket. seat is the seat serve takes and releases.
	credentialID string
	seat         seat
	// redeem spends the ticket that opened the stream; nil for a bearer stream. It runs after the
	// seat is taken, so a refused connect never spends it.
	redeem func(context.Context) error
}

// serve opens a pending subscription, authorizes, grants, takes a seat, spends the ticket (if any),
// and upgrades. Everything that can fail does so before the upgrade, as an ordinary HTTP error.
// The seat comes after authorization so a caller who may not see the scope costs the lease store
// nothing, and the ticket after the seat so a refusal leaves it redeemable.
func (h streamHandlers) serve(c *gin.Context, a serveArgs) {
	if hook := h.opts.onServe; hook != nil {
		hook()
	}
	ctx := c.Request.Context()
	released := false
	defer func() {
		if !released {
			h.release(a.seat, false) // refused before the upgrade
		}
	}()
	if h.svc.Hub.ShuttingDown() {
		writeError(c, stream.ErrShuttingDown, nil)
		return
	}
	sub := h.svc.Hub.Open(a.session, a.actor, a.encounter, h.opts.buffer())
	defer sub.Close()
	v, err := a.authorize(ctx)
	if err == nil {
		err = sub.Grant(v)
		if errors.Is(err, stream.ErrEvicted) && sub.ServerStopping() {
			err = stream.ErrShuttingDown // closed by Shutdown after the check above
		} else if errors.Is(err, stream.ErrEvicted) || errors.Is(err, stream.ErrViewer) {
			err = data.ErrNotMember
		}
	}
	if err != nil {
		writeError(c, err, nil)
		return
	}
	if a.seat, err = h.takeSeat(ctx, a); err != nil {
		writeError(c, err, nil)
		return
	}
	if a.redeem != nil {
		if err = a.redeem(ctx); err != nil { // lost a race for the ticket
			writeError(c, err, nil)
			return
		}
	}
	heads, truncated, err := h.svc.Streams.Heads(ctx, v, a.encounter)
	if err != nil {
		writeError(c, err, nil)
		return
	}
	keep := false // only a dropped connection keeps its seat for the grace period
	released = true
	defer func() { h.release(a.seat, keep) }()
	conn, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{OriginPatterns: h.opts.AllowedOrigins})
	if err != nil {
		return // Accept has answered
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(readLimit) // clients send nothing but control frames
	cl := &connection{h: h, conn: conn, sub: sub, v: v, a: a}
	if err := cl.hello(ctx, heads, truncated); err != nil {
		return
	}
	keep = cl.run(ctx)
}

type headFrame struct {
	Type string `json:"type"`
	domain.Head
}

// seat is a stream's place in the lease store; the zero seat means none is kept (no store, or no
// cap configured).
type seat struct {
	lease data.StreamLease
	held  bool
}

// takeSeat gives the stream a seat in its bucket: the user's for a user's stream, the credential's
// own for an integration credential's, so a leaked or runaway credential cannot lock its owner out
// of their own stream. Over the cap is data.ErrStreamLimit; a store failure is
// data.ErrStreamUnavailable and a new connect fails closed (streams already open stay up).
func (h streamHandlers) takeSeat(ctx context.Context, a serveArgs) (seat, error) {
	max, bucket := h.opts.ConcurrentMax, "user:"+a.actor
	if a.credentialID != "" {
		max, bucket = h.opts.CredentialMax, "cred:"+a.credentialID
	}
	if h.svc.Leases == nil || max <= 0 {
		return seat{}, nil
	}
	l := data.StreamLease{ID: randomHex(), Bucket: bucket, UserID: a.actor, SessionID: a.session, ClientID: a.clientID, Pod: h.opts.Pod}
	if err := h.svc.Leases.Acquire(ctx, l, h.opts.leaseTTL(), max); err != nil {
		if errors.Is(err, data.ErrStreamUnavailable) {
			slog.WarnContext(ctx, "the stream seat store is unavailable; refusing a new stream", "error", err)
		}
		return seat{}, err
	}
	return seat{l, true}, nil
}

// release ends a seat. Only a connection that dropped (keep) holds it for the grace period; one the
// server ended, or that never opened, frees it now.
func (h streamHandlers) release(s seat, keep bool) {
	if !s.held {
		return
	}
	if err := h.svc.Leases.Release(context.Background(), s.lease, h.opts.Grace, keep); err != nil {
		slog.Warn("releasing a stream seat failed; it lapses on its own", "seat", s.lease.ID, "error", err)
	}
}

// admit takes one of this pod's connection slots, or reports the pod full. A compare-and-swap, so a
// refused attempt never counts.
func (h streamHandlers) admit() (release func(), ok bool) {
	max := int64(h.opts.MaxPerPod)
	if max <= 0 {
		return func() {}, true
	}
	for {
		n := h.open.Load()
		if n >= max {
			return nil, false
		}
		if h.open.CompareAndSwap(n, n+1) {
			return func() { h.open.Add(-1) }, true
		}
	}
}

// refuseFull answers a holder of a real ticket (or an authenticated caller) on a full pod: it
// accepts the upgrade and closes with 1013, since a browser cannot read the status of a refused
// upgrade. The ticket was only peeked, so the client keeps it. Each refusal holds a socket for the
// close handshake, so they are bounded; past the bound the answer is a plain 503.
func (h streamHandlers) refuseFull(c *gin.Context) {
	if h.refusing != nil {
		select {
		case h.refusing <- struct{}{}:
			defer func() { <-h.refusing }()
		default:
			c.Header("Retry-After", "5")
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, ErrorBody{Code: "pod_full", Message: "This server is full; try again shortly.", CorrelationID: CorrelationID(c)})
			return
		}
	}
	conn, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{OriginPatterns: h.opts.AllowedOrigins})
	if err != nil {
		return
	}
	_ = conn.Close(websocket.StatusTryAgainLater, "pod full")
}

func randomHex() string {
	b := make([]byte, 12)
	_, _ = cryptorand.Read(b)
	return hex.EncodeToString(b)
}

type connection struct {
	h    streamHandlers
	conn *websocket.Conn
	sub  *stream.Subscription
	v    data.Viewer
	a    serveArgs

	renewFailing bool
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
func (cl *connection) hello(ctx context.Context, heads []domain.Head, truncated bool) error {
	opts := cl.h.opts
	if err := cl.write(ctx, gin.H{
		"type": "hello", "session_id": cl.a.session, "encounter_id": cl.a.encounter, "client_id": cl.a.clientID,
		"heartbeat_seconds": int(opts.heartbeat().Seconds()), "max_age_seconds": int(opts.maxAge().Seconds()), "heads_truncated": truncated,
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
func (cl *connection) run(ctx context.Context) (keepSeat bool) {
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
				if cl.sub.ServerStopping() {
					_ = cl.conn.Close(websocket.StatusGoingAway, "server restarting")
				} else if cl.sub.Overflowed() {
					_ = cl.conn.Close(CloseSlowConsumer, "slow consumer")
				} else {
					_ = cl.conn.Close(CloseMembershipChanged, "membership changed")
				}
				return
			}
			if hook := cl.h.opts.beforeWrite; hook != nil {
				hook()
			}
			if cl.sub.Ended() { // evicted: what is still buffered must not be written
				continue
			}
			if cl.write(ctx, ev) != nil {
				return true
			}
		case <-tick.C:
			// Check first, so a revoked connection gets no further frame.
			if code, why := cl.check(ctx); code != 0 {
				_ = cl.conn.Close(code, why)
				return false
			}
			if cl.write(ctx, gin.H{"type": "heartbeat"}) != nil {
				return true
			}
			select {
			case pings <- struct{}{}:
			default:
			}
			cl.renew(ctx)
		case <-maxAge.C:
			_ = cl.conn.Close(CloseMaxAge, "maximum connection age")
			return false
		case <-ctx.Done():
			return true
		}
	}
}

// renew keeps the stream's seat alive, bounded so a hung store cannot stall the heartbeat, the
// membership re-check or the credential guard. A failure leaves the stream up; the seat lapses after
// the lease TTL if renewals keep failing, and the next connect for that user then has room.
func (cl *connection) renew(ctx context.Context) {
	if !cl.a.seat.held {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, cl.h.opts.heartbeat()/2)
	defer cancel()
	err := cl.h.svc.Leases.Renew(ctx, cl.a.seat.lease, cl.h.opts.leaseTTL())
	if err != nil && !cl.renewFailing {
		slog.WarnContext(ctx, "renewing a stream seat failed; it lapses if renewals keep failing", "seat", cl.a.seat.lease.ID, "error", err)
	}
	cl.renewFailing = err != nil // log the first failure of a run, not every heartbeat
	if hook := cl.h.opts.afterRenew; hook != nil {
		hook()
	}
}

// check re-reads what the stream was authorized on. A failed membership read keeps the stream (the
// next check retries; the maximum age bounds a persistent failure). A credential's failed reads
// close it after maxGuardErrors in a row.
func (cl *connection) check(ctx context.Context) (websocket.StatusCode, string) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout) // a hung store must not stall delivery
	defer cancel()
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
