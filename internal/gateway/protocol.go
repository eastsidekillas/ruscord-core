// Package gateway serves the realtime WebSocket: one connection per device
// that authenticates with a ticket, receives a READY snapshot and then the
// events it is allowed to see, in order (docs/adr/0016-gateway.md).
package gateway

import (
	"encoding/json"

	"github.com/coder/websocket"
)

// Version is the protocol version clients put in the URL: /gateway?v=1.
const Version = "1"

// Opcodes. A frame is {"op", "d", "s"?, "t"?}; s and t are set on DISPATCH only.
const (
	opDispatch     = 0  // ← event; s is the session sequence number
	opHeartbeat    = 1  // → d: the last s the client received, or null
	opIdentify     = 2  // → d: {ticket}
	opPresence     = 3  // → d: {status}: this device is "online" (in use) or "idle"
	opResume       = 6  // → d: {ticket, session_id, seq}
	opHello        = 10 // ← d: {heartbeat_interval} (milliseconds), first frame
	opHeartbeatAck = 11 // ← reply to every heartbeat
)

// Dispatch types the gateway makes itself; the rest come from internal/events.
const (
	typeReady   = "READY"
	typeResumed = "RESUMED"
	// typePermissionsUpdate carries recomputed permissions after a change
	// of roles, overwrites, the own member or the owner.
	typePermissionsUpdate = "GUILD_PERMISSIONS_UPDATE"
)

// Close codes. What the client does next depends on the code; docs/api.md
// has the table.
const (
	closeUnknownError         websocket.StatusCode = 4000                           // resume
	closeUnknownOpcode        websocket.StatusCode = 4001                           // resume
	closeDecodeError          websocket.StatusCode = 4002                           // resume
	closeNotAuthenticated     websocket.StatusCode = 4003                           // identify
	closeAuthFailed           websocket.StatusCode = 4004                           // new ticket, identify; sign in if the ticket is refused
	closeAlreadyAuthenticated websocket.StatusCode = 4005                           // resume
	closeSessionInvalid       websocket.StatusCode = 4006                           // identify
	closeRateLimited          websocket.StatusCode = 4008                           // resume after a pause
	closeSessionTimedOut      websocket.StatusCode = 4009                           // resume
	closeSlowConsumer         websocket.StatusCode = 4010                           // resume
	closeInvalidVersion       websocket.StatusCode = 4012                           // do not reconnect
	closeTooManySessions      websocket.StatusCode = 4013                           // do not reconnect automatically
	closeRestart              websocket.StatusCode = websocket.StatusServiceRestart // resume
)

// endsSession lists the closes after which the session cannot be resumed.
// Every other close keeps it for the resume window.
func endsSession(code websocket.StatusCode) bool {
	switch code {
	case closeNotAuthenticated, closeAuthFailed, closeSessionInvalid, closeInvalidVersion,
		closeTooManySessions, websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return true
	}
	return false
}

// Limits on what a client sends: frames are tiny (heartbeats, identify).
const (
	maxInboundBytes    = 4096
	maxInboundPerMin   = 120
	maxTicketLen       = 64
	maxSessionIDLength = 64
)

type outFrame struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  uint64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

type inFrame struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
}

type identifyData struct {
	Ticket string `json:"ticket"`
}

type resumeData struct {
	Ticket    string `json:"ticket"`
	SessionID string `json:"session_id"`
	Seq       uint64 `json:"seq"`
}

func encode(f outFrame) []byte {
	if f.D == nil {
		f.D = json.RawMessage("null")
	}
	return mustJSON(f)
}

func dispatchFrame(seq uint64, d dispatch) []byte {
	return encode(outFrame{Op: opDispatch, S: seq, T: d.T, D: d.D})
}
