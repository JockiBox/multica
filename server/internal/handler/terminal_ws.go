package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/daemonws"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"

	"github.com/golang-jwt/jwt/v5"
)

// terminalWriteWait caps how long a single WriteMessage may block before
// we tear the browser connection down as slow-client. Matches the daemonws
// hub's writeWait so back-pressure semantics are consistent end-to-end.
const terminalWriteWait = 10 * time.Second

// terminalOpenTimeout caps how long the proxy waits for the daemon to
// respond to a terminal.open request with terminal.opened or terminal.error.
// 5s is generous: PTY spawn is local and synchronous on the daemon side.
const terminalOpenTimeout = 5 * time.Second

// terminalUpgrader reuses the realtime hub's origin allowlist. The terminal
// endpoint executes a shell on the daemon, so it must be at least as strict
// about cross-origin connections as the read-only realtime WS — using the
// shared CheckOrigin keeps the policy in one place and prevents an
// accidentally permissive `CheckOrigin: true` from sneaking past review.
var terminalUpgrader = websocket.Upgrader{
	CheckOrigin: realtime.CheckOrigin,
}

// HandleIssueTerminalWS proxies a browser WebSocket onto a PTY running on
// the daemon hosting the issue's most-recent agent task. The flow per
// connection:
//
//  1. Authenticate the user (cookie JWT preferred; first-message auth as
//     fallback for clients that cannot set cookies — e.g. some Desktop
//     dev modes).
//  2. Resolve issue → workspace → latest task with a non-empty work_dir +
//     runtime_id. Fail closed if no such task exists; users see a clear
//     "no task to attach to" error instead of a silent hang.
//  3. Register a sink on the daemonws TerminalRouter under a fresh
//     request_id, then send terminal.open to the daemon.
//  4. On terminal.opened: re-register the sink under the session_id the
//     daemon picked, drop the request_id route, and start the bidirectional
//     pump until either side closes.
//  5. On disconnect: send terminal.close so the daemon tears the PTY down
//     promptly rather than waiting for its idle sweep.
func (h *Handler) HandleIssueTerminalWS(w http.ResponseWriter, r *http.Request) {
	if h.DaemonHub == nil {
		http.Error(w, `{"error":"terminal proxy not configured"}`, http.StatusServiceUnavailable)
		return
	}

	workspaceID := r.URL.Query().Get("workspace_id")
	if workspaceID == "" {
		http.Error(w, `{"error":"workspace_id required"}`, http.StatusBadRequest)
		return
	}
	wsUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		http.Error(w, `{"error":"invalid workspace_id"}`, http.StatusBadRequest)
		return
	}

	issueParam := chi.URLParam(r, "issue_id")
	if issueParam == "" {
		http.Error(w, `{"error":"issue_id required"}`, http.StatusBadRequest)
		return
	}

	userID, errMsg := terminalAuthCookie(r, h)
	if errMsg != "" && userID == "" {
		// No cookie or invalid cookie. Defer auth to the first WS frame.
	}
	if userID != "" && !h.terminalIsMember(r.Context(), userID, workspaceID) {
		http.Error(w, `{"error":"not a member of this workspace"}`, http.StatusForbidden)
		return
	}

	conn, err := terminalUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("terminal ws upgrade failed", "error", err)
		return
	}

	if userID == "" {
		uid, errMsg := terminalFirstFrameAuth(conn, h)
		if errMsg != "" {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(errMsg))
			conn.Close()
			return
		}
		if !h.terminalIsMember(r.Context(), uid, workspaceID) {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"error":"not a member of this workspace"}`))
			conn.Close()
			return
		}
		userID = uid
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"auth_ack"}`))
	}

	issue, ok := h.terminalResolveIssue(r.Context(), issueParam, wsUUID)
	if !ok {
		sendTerminalErrorAndClose(conn, "", "", protocol.TerminalErrorCodeTaskNotFound, "issue not found")
		return
	}

	task, ok := h.terminalLatestAttachableTask(r.Context(), issue.ID)
	if !ok {
		sendTerminalErrorAndClose(conn, "", "", protocol.TerminalErrorCodeTaskNotFound, "no agent task has run on this issue yet — trigger a run first")
		return
	}

	cols := parseUint16Query(r, "cols", 80)
	rows := parseUint16Query(r, "rows", 24)

	proxy := newTerminalProxy(conn, h.DaemonHub, userID, util.UUIDToString(task.RuntimeID), util.UUIDToString(task.ID), workspaceID, util.UUIDToString(issue.ID), task.SessionID.String, task.WorkDir.String, cols, rows)
	proxy.audit = newTerminalAuditRecorder(h, util.UUIDToString(issue.ID), util.UUIDToString(task.ID), util.UUIDToString(task.RuntimeID), workspaceID, userID, task.WorkDir.String)
	proxy.run()
}

// terminalAuditRecorder writes terminal_sessions rows for the audit log
// (RFC §Auth) and as the data source behind `multica issue runs`'
// `type=terminal` entries. Persisting happens out-of-band on a background
// context so a slow DB write can't stall the WS handshake — the trade-off
// is that an audit row may briefly lag the actual session open by a few
// milliseconds, which is acceptable for an audit surface.
type terminalAuditRecorder struct {
	h           *Handler
	issueID     string
	taskID      string
	runtimeID   string
	workspaceID string
	userID      string
	workDir     string
}

func newTerminalAuditRecorder(h *Handler, issueID, taskID, runtimeID, workspaceID, userID, workDir string) *terminalAuditRecorder {
	if h == nil || h.Queries == nil {
		// Tests that build a handler without DB queries (terminal protocol
		// tests, etc.) skip recording — keep that path nil-safe so the
		// audit hook never panics in a unit environment.
		return nil
	}
	return &terminalAuditRecorder{
		h:           h,
		issueID:     issueID,
		taskID:      taskID,
		runtimeID:   runtimeID,
		workspaceID: workspaceID,
		userID:      userID,
		workDir:     workDir,
	}
}

func (a *terminalAuditRecorder) RecordOpen(sessionID, shell string) {
	if a == nil {
		return
	}
	sessUUID, err := util.ParseUUID(sessionID)
	if err != nil {
		slog.Debug("terminal audit: invalid session id", "session_id", sessionID, "error", err)
		return
	}
	issueUUID, err := util.ParseUUID(a.issueID)
	if err != nil {
		return
	}
	taskUUID, err := util.ParseUUID(a.taskID)
	if err != nil {
		return
	}
	wsUUID, err := util.ParseUUID(a.workspaceID)
	if err != nil {
		return
	}
	userUUID, err := util.ParseUUID(a.userID)
	if err != nil {
		return
	}
	var runtimeUUID pgtype.UUID
	if a.runtimeID != "" {
		if parsed, perr := util.ParseUUID(a.runtimeID); perr == nil {
			runtimeUUID = parsed
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.h.Queries.CreateTerminalSession(ctx, db.CreateTerminalSessionParams{
		ID:          sessUUID,
		WorkspaceID: wsUUID,
		IssueID:     issueUUID,
		TaskID:      taskUUID,
		RuntimeID:   runtimeUUID,
		UserID:      userUUID,
		WorkDir:     a.workDir,
		Shell:       shell,
		StartedAt:   pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	}); err != nil {
		slog.Warn("terminal audit: record open failed", "session_id", sessionID, "error", err)
	}
}

func (a *terminalAuditRecorder) RecordClose(sessionID string, exitCode int32, hasExit bool, reason string) {
	if a == nil || sessionID == "" {
		return
	}
	sessUUID, err := util.ParseUUID(sessionID)
	if err != nil {
		return
	}
	var code pgtype.Int4
	if hasExit {
		code = pgtype.Int4{Int32: exitCode, Valid: true}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.h.Queries.CloseTerminalSession(ctx, db.CloseTerminalSessionParams{
		ID:          sessUUID,
		EndedAt:     pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		ExitCode:    code,
		CloseReason: reason,
	}); err != nil {
		slog.Warn("terminal audit: record close failed", "session_id", sessionID, "error", err)
	}
}

// terminalProxy is the per-connection bridge between browser and daemon.
// Methods that touch the WS connection do so from one of two goroutines
// only: writePump owns conn writes and readPump owns conn reads, matching
// gorilla/websocket's single-goroutine-per-direction contract.
type terminalProxy struct {
	conn        *websocket.Conn
	hub         *daemonws.Hub
	userID      string
	runtimeID   string
	taskID      string
	workspaceID string
	issueID     string
	priorSess   string
	workDir     string
	cols        uint16
	rows        uint16

	requestID string

	mu         sync.Mutex
	sessionID  string
	closedOnce sync.Once
	closeCh    chan struct{}
	sendCh     chan []byte

	openedCh chan struct{}
	openErr  chan terminalOpenFailure

	// audit is the persistence hook for terminal_sessions rows (RFC §Auth).
	// nil in tests that build a proxy without a Handler — every call site
	// is nil-safe through the recorder methods.
	audit *terminalAuditRecorder

	// exitMu guards exitCode/hasExit, written from writePump when the
	// daemon sends terminal.exit and read from the run() defer that
	// finalizes the audit row.
	exitMu   sync.Mutex
	exitCode int32
	hasExit  bool
	exitMsg  string
}

type terminalOpenFailure struct {
	code    string
	message string
}

func newTerminalProxy(conn *websocket.Conn, hub *daemonws.Hub, userID, runtimeID, taskID, workspaceID, issueID, priorSess, workDir string, cols, rows uint16) *terminalProxy {
	return &terminalProxy{
		conn:        conn,
		hub:         hub,
		userID:      userID,
		runtimeID:   runtimeID,
		taskID:      taskID,
		workspaceID: workspaceID,
		issueID:     issueID,
		priorSess:   priorSess,
		workDir:     workDir,
		cols:        cols,
		rows:        rows,
		requestID:   uuid.NewString(),
		closeCh:     make(chan struct{}),
		sendCh:      make(chan []byte, 256),
		openedCh:    make(chan struct{}),
		openErr:     make(chan terminalOpenFailure, 1),
	}
}

// Deliver implements daemonws.TerminalSink. The daemon hub's read pump
// invokes this for every terminal.* frame addressed to our request_id /
// session_id. We must stay non-blocking; the hub drops the frame on a
// full buffer rather than stalling its own pump.
func (p *terminalProxy) Deliver(frame []byte) bool {
	select {
	case p.sendCh <- frame:
		return true
	default:
		return false
	}
}

func (p *terminalProxy) run() {
	defer p.conn.Close()

	router := p.hub.TerminalRouter()
	router.Register(p.requestID, p)
	defer router.Unregister(p.requestID)

	go p.writePump()

	if err := p.sendOpenToDaemon(); err != nil {
		// Couldn't reach the daemon at all — surface a structured error and
		// bail before we register cleanup paths that assume a live session.
		sendTerminalErrorOverChannel(p.sendCh, p.requestID, "", protocol.TerminalErrorCodeInternal, err.Error())
		<-time.After(50 * time.Millisecond) // give writePump a tick to flush
		p.shutdown()
		return
	}

	// Block until the open ack arrives, the open is rejected, or the user
	// disconnects mid-handshake. After this point sessionID is stable and
	// we are routing on session_id rather than request_id.
	select {
	case <-p.openedCh:
		// Open succeeded. The router is already re-keyed on session_id by
		// observeOpen. Fall through to the bidirectional pump.
	case failure := <-p.openErr:
		sendTerminalErrorOverChannel(p.sendCh, p.requestID, "", failure.code, failure.message)
		<-time.After(50 * time.Millisecond)
		p.shutdown()
		return
	case <-p.closeCh:
		return
	case <-time.After(terminalOpenTimeout):
		sendTerminalErrorOverChannel(p.sendCh, p.requestID, "", protocol.TerminalErrorCodeInternal, "daemon did not respond to terminal.open within timeout")
		<-time.After(50 * time.Millisecond)
		p.shutdown()
		return
	}

	defer func() {
		sid := p.SessionID()
		if sid != "" {
			router.Unregister(sid)
			// Best-effort teardown on the daemon. If the connection to the
			// daemon is already gone, the daemon's own clearWSWrites path
			// will close the session — we just lose an idle slot's worth of
			// latency before the GC catches it.
			frame, err := marshalTerminalFrame(protocol.MessageTypeTerminalClose, protocol.TerminalClosePayload{
				SessionID: sid,
				Reason:    "browser_disconnect",
			})
			if err == nil {
				_ = p.hub.SendToRuntime(p.runtimeID, frame)
			}
			// Stamp the audit row. If the daemon sent terminal.exit before
			// we got here, use its exit code + reason; otherwise this is a
			// browser-initiated disconnect.
			p.exitMu.Lock()
			code, has, reason := p.exitCode, p.hasExit, p.exitMsg
			p.exitMu.Unlock()
			if reason == "" {
				reason = "browser_disconnect"
			}
			p.audit.RecordClose(sid, code, has, reason)
		}
	}()

	p.readPump()
}

func (p *terminalProxy) SessionID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessionID
}

func (p *terminalProxy) sendOpenToDaemon() error {
	payload := protocol.TerminalOpenPayload{
		RequestID:      p.requestID,
		TaskID:         p.taskID,
		WorkspaceID:    p.workspaceID,
		UserID:         p.userID,
		IssueID:        p.issueID,
		WorkDir:        p.workDir,
		PriorSessionID: p.priorSess,
		Cols:           p.cols,
		Rows:           p.rows,
	}
	frame, err := marshalTerminalFrame(protocol.MessageTypeTerminalOpen, payload)
	if err != nil {
		return err
	}
	if err := p.hub.SendToRuntime(p.runtimeID, frame); err != nil {
		if errors.Is(err, daemonws.ErrNoDaemonForRuntime) {
			return errors.New("daemon offline for this runtime — start the agent's daemon and retry")
		}
		return err
	}
	return nil
}

// readPump reads browser frames and forwards terminal.data/resize/close to
// the daemon. Unknown frames are dropped silently. Returns when the
// connection closes; we then trigger shutdown so writePump exits too.
func (p *terminalProxy) readPump() {
	defer p.shutdown()
	p.conn.SetReadLimit(64 * 1024)
	for {
		_, raw, err := p.conn.ReadMessage()
		if err != nil {
			return
		}
		var env protocol.Message
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		sid := p.SessionID()
		// We force-stamp session_id on outbound frames: the browser may have
		// sent its own session_id, but only the one the daemon assigned is
		// trusted. This prevents a misbehaving client from addressing a
		// session that another user opened against the same daemon.
		switch env.Type {
		case protocol.MessageTypeTerminalData:
			var pl protocol.TerminalDataPayload
			if err := json.Unmarshal(env.Payload, &pl); err != nil {
				continue
			}
			pl.SessionID = sid
			frame, err := marshalTerminalFrame(protocol.MessageTypeTerminalData, pl)
			if err != nil {
				continue
			}
			_ = p.hub.SendToRuntime(p.runtimeID, frame)
		case protocol.MessageTypeTerminalResize:
			var pl protocol.TerminalResizePayload
			if err := json.Unmarshal(env.Payload, &pl); err != nil {
				continue
			}
			pl.SessionID = sid
			frame, err := marshalTerminalFrame(protocol.MessageTypeTerminalResize, pl)
			if err != nil {
				continue
			}
			_ = p.hub.SendToRuntime(p.runtimeID, frame)
		case protocol.MessageTypeTerminalClose:
			return
		}
	}
}

// writePump is the single owner of conn writes. It also watches for the
// terminal.opened / terminal.error handshake transition while it pumps,
// because separating those into a second goroutine would race over
// sendCh (only one reader per channel value).
//
// Once the session is open, this is just a straight relay. During the
// open window (sessionID empty), every frame is forwarded to the browser
// AND inspected — opened/error frames feed openedCh / openErr so run()
// can unblock or fail the handshake.
func (p *terminalProxy) writePump() {
	router := p.hub.TerminalRouter()
	for {
		select {
		case <-p.closeCh:
			return
		case frame, ok := <-p.sendCh:
			if !ok {
				return
			}
			p.forwardToBrowser(frame)
			// terminal.exit can arrive at any point in the session lifecycle
			// (idle timeout, child crash, manager shutdown), so we always
			// peek the envelope to capture the exit code for the audit row.
			// terminal.opened / terminal.error are only meaningful during
			// the open handshake window — once sessionID is set they are
			// just relayed without re-inspection.
			var env protocol.Message
			if err := json.Unmarshal(frame, &env); err != nil {
				continue
			}
			if env.Type == protocol.MessageTypeTerminalExit {
				var ep protocol.TerminalExitPayload
				if err := json.Unmarshal(env.Payload, &ep); err == nil {
					p.exitMu.Lock()
					p.exitCode = int32(ep.ExitCode)
					p.hasExit = true
					if ep.Reason != "" {
						p.exitMsg = ep.Reason
					}
					p.exitMu.Unlock()
					// Finalize the audit row as soon as the daemon reports
					// exit, not when the client disconnects. CloseTerminalSession
					// is idempotent (WHERE ended_at IS NULL) so the browser_disconnect
					// fallback in run()'s defer becomes a no-op if it fires later.
					// Without this, a client that keeps the WS open after exit
					// would leave terminal_sessions.ended_at NULL forever and
					// `multica issue runs` would render an already-exited
					// terminal as active.
					sid := ep.SessionID
					if sid == "" {
						sid = p.SessionID()
					}
					reason := ep.Reason
					if reason == "" {
						reason = "exited"
					}
					p.audit.RecordClose(sid, int32(ep.ExitCode), true, reason)
				}
				continue
			}
			if p.SessionID() != "" {
				continue
			}
			switch env.Type {
			case protocol.MessageTypeTerminalOpened:
				var op protocol.TerminalOpenedPayload
				if err := json.Unmarshal(env.Payload, &op); err == nil && op.SessionID != "" {
					p.mu.Lock()
					p.sessionID = op.SessionID
					p.mu.Unlock()
					router.Register(op.SessionID, p)
					router.Unregister(p.requestID)
					// Persist the open audit row before unblocking run(). If
					// the DB write fails the slog records the error; the
					// session itself still runs (audit is best-effort).
					p.audit.RecordOpen(op.SessionID, op.Shell)
					close(p.openedCh)
				}
			case protocol.MessageTypeTerminalError:
				var ep protocol.TerminalErrorPayload
				if err := json.Unmarshal(env.Payload, &ep); err == nil {
					select {
					case p.openErr <- terminalOpenFailure{code: ep.Code, message: ep.Message}:
					default:
					}
				}
			}
		}
	}
}

func (p *terminalProxy) forwardToBrowser(frame []byte) {
	p.conn.SetWriteDeadline(time.Now().Add(terminalWriteWait))
	if err := p.conn.WriteMessage(websocket.TextMessage, frame); err != nil {
		slog.Debug("terminal ws write to browser failed", "error", err)
		p.shutdown()
	}
}

func (p *terminalProxy) shutdown() {
	p.closedOnce.Do(func() {
		close(p.closeCh)
	})
}

func sendTerminalErrorOverChannel(ch chan<- []byte, requestID, sessionID, code, message string) {
	frame, err := marshalTerminalFrame(protocol.MessageTypeTerminalError, protocol.TerminalErrorPayload{
		RequestID: requestID,
		SessionID: sessionID,
		Code:      code,
		Message:   message,
	})
	if err != nil {
		return
	}
	select {
	case ch <- frame:
	default:
	}
}

func sendTerminalErrorAndClose(conn *websocket.Conn, requestID, sessionID, code, message string) {
	frame, err := marshalTerminalFrame(protocol.MessageTypeTerminalError, protocol.TerminalErrorPayload{
		RequestID: requestID,
		SessionID: sessionID,
		Code:      code,
		Message:   message,
	})
	if err == nil {
		_ = conn.WriteMessage(websocket.TextMessage, frame)
	}
	conn.Close()
}

func marshalTerminalFrame(msgType string, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(protocol.Message{Type: msgType, Payload: raw})
}

func parseUint16Query(r *http.Request, key string, defaultVal uint16) uint16 {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return defaultVal
	}
	v, err := strconv.ParseUint(raw, 10, 16)
	if err != nil || v == 0 {
		return defaultVal
	}
	return uint16(v)
}

// --- auth helpers (cookie + first-frame JWT/PAT) ---

func terminalAuthCookie(r *http.Request, h *Handler) (string, string) {
	cookie, err := r.Cookie(auth.AuthCookieName)
	if err != nil || cookie.Value == "" {
		return "", ""
	}
	return terminalAuthToken(cookie.Value, h, r.Context())
}

func terminalFirstFrameAuth(conn *websocket.Conn, h *Handler) (string, string) {
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return "", `{"error":"auth timeout or read error"}`
	}
	var msg struct {
		Type    string `json:"type"`
		Payload struct {
			Token string `json:"token"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil || msg.Type != "auth" || msg.Payload.Token == "" {
		return "", `{"error":"expected auth message as first frame"}`
	}
	return terminalAuthToken(msg.Payload.Token, h, context.Background())
}

func terminalAuthToken(tokenStr string, h *Handler, ctx context.Context) (string, string) {
	if len(tokenStr) > 4 && tokenStr[:4] == "mul_" {
		if h.PATCache == nil {
			pat, err := h.Queries.GetPersonalAccessTokenByHash(ctx, auth.HashToken(tokenStr))
			if err != nil {
				return "", `{"error":"invalid token"}`
			}
			return util.UUIDToString(pat.UserID), ""
		}
		hash := auth.HashToken(tokenStr)
		if uid, ok := h.PATCache.Get(ctx, hash); ok {
			return uid, ""
		}
		pat, err := h.Queries.GetPersonalAccessTokenByHash(ctx, hash)
		if err != nil {
			return "", `{"error":"invalid token"}`
		}
		uid := util.UUIDToString(pat.UserID)
		return uid, ""
	}
	token, err := jwt.Parse(tokenStr, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return auth.JWTSecret(), nil
	})
	if err != nil || !token.Valid {
		return "", `{"error":"invalid token"}`
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", `{"error":"invalid claims"}`
	}
	uid, ok := claims["sub"].(string)
	if !ok || uid == "" {
		return "", `{"error":"invalid claims"}`
	}
	return uid, ""
}

func (h *Handler) terminalIsMember(ctx context.Context, userID, workspaceID string) bool {
	userUUID, err := util.ParseUUID(userID)
	if err != nil {
		return false
	}
	wsUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return false
	}
	_, err = h.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID:      userUUID,
		WorkspaceID: wsUUID,
	})
	return err == nil
}

func (h *Handler) terminalResolveIssue(ctx context.Context, issueID string, wsUUID pgtype.UUID) (db.Issue, bool) {
	if parts := splitIdentifier(issueID); parts != nil {
		issue, err := h.Queries.GetIssueByNumber(ctx, db.GetIssueByNumberParams{
			WorkspaceID: wsUUID,
			Number:      parts.number,
		})
		if err == nil {
			return issue, true
		}
	}
	issueUUID, err := util.ParseUUID(issueID)
	if err != nil {
		return db.Issue{}, false
	}
	issue, err := h.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{
		ID:          issueUUID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		return db.Issue{}, false
	}
	return issue, true
}

// terminalLatestAttachableTask returns the most recent task on the issue
// that the proxy can attach to: must have a known work_dir, a runtime_id,
// and a daemon currently connected for that runtime. Falls back through
// the task history rather than picking only the absolute latest, because
// the most-recent row may be a queued task that never ran (no workdir yet).
func (h *Handler) terminalLatestAttachableTask(ctx context.Context, issueID pgtype.UUID) (db.AgentTaskQueue, bool) {
	tasks, err := h.Queries.ListTasksByIssue(ctx, issueID)
	if err != nil {
		return db.AgentTaskQueue{}, false
	}
	for _, t := range tasks {
		if !t.WorkDir.Valid || t.WorkDir.String == "" {
			continue
		}
		if !t.RuntimeID.Valid {
			continue
		}
		return t, true
	}
	return db.AgentTaskQueue{}, false
}
