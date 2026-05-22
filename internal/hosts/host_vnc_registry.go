package hosts

// Host module — VNC skill in-memory session registry.
//
// Mirrors host_shell_registry.go but with VNC-specific knobs:
//   - Per-host cap is 2 (VNC pushes full framebuffer streams; two
//     simultaneous sessions per host is plenty; cap exists to keep a
//     stuck refresh from spawning a stack of TCP relays to :5900).
//   - Global cap from POLAR_MAX_VNC_SESSIONS (default 16).
//   - No backscroll. The shell skill replays the last 64 KiB of stdout
//     so a refreshing browser sees recent terminal output; for VNC,
//     mid-stream RFB bytes are partial framebuffer deltas and would
//     desync the noVNC decoder. New attach = new RFB handshake.
//
// Registry is not persisted — dock restart drops every active session;
// the agent-side TCP socket closes the moment the WS disconnect cleanup
// fires in loop.go. Operators don't expect VNC sessions to survive a
// dock bounce.

import (
	"errors"
	"sync"
	"time"
)

const perHostVNCCap = 2

// vncHandshakeBufferBytes is the early-byte ring kept until the browser
// WS bridge first attaches. The RFB protocol's initial handshake fits
// in ~50 bytes (server version → security types → security result →
// ServerInit name+desktop info), so 16 KiB is comfortably more than
// enough to survive the race between agent.net.Dial succeeding and
// the browser's WS upgrade attaching a bridge. Without this buffer,
// the agent's reader emits the RFB version banner immediately, the
// supervisor sees bridge==nil, drops the bytes, macOS Screen Sharing
// waits for the client reply that never comes, and ~8s later closes
// the TCP conn → "stuck on connecting" forever.
const vncHandshakeBufferBytes = 16 * 1024

var (
	errVNCPerHostCapReached = errors.New("vnc session cap reached for this host (2)")
	errVNCGlobalCapReached  = errors.New("global vnc session cap reached")
	errVNCSessionNotFound   = errors.New("vnc session not found")
)

// vncSession is the dock-side handle for one open VNC relay. Created
// by handleHostVNCOpen and persists for the run's full lifetime (NOT
// just for any one browser's WS). Refresh / re-pick reattaches without
// spawning a fresh TCP dial to the VNC server.
type vncSession struct {
	RunID          int64     `json:"run_id"`
	HostID         string    `json:"host_id"`
	HostSkillID    int64     `json:"host_skill_id"`
	OperatorUserID string    `json:"operator_user_id"`
	OpenedAt       time.Time `json:"opened_at"`
	Target         string    `json:"target"`

	bridge *vncBridge `json:"-"`

	// handshakeBuf captures stdout bytes from the agent (RFB version,
	// security negotiation, ServerInit) before the browser WS bridge
	// first attaches. Once bridgeAttached is set we stop buffering and
	// trust the live forwardStdout path — mid-stream framebuffer bytes
	// would desync noVNC if replayed after that point anyway.
	handshakeBuf    *byteRing
	bridgeAttached  bool
}

// vncBridge is the dock-side handle to one attached browser. Pointer
// identity matters for the attach/detach race-free contract — same as
// shellBridge.
type vncBridge struct {
	cancel     func()
	eventSink  chan skillEventFrame
	stdoutSink chan []byte
}

type vncSessionRegistry struct {
	mu        sync.Mutex
	globalCap int
	byHost    map[string][]*vncSession
	byRun     map[int64]*vncSession
}

func newVNCSessionRegistry(globalCap int) *vncSessionRegistry {
	return &vncSessionRegistry{
		globalCap: globalCap,
		byHost:    map[string][]*vncSession{},
		byRun:     map[int64]*vncSession{},
	}
}

func (r *vncSessionRegistry) reserve(hostID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.byRun) >= r.globalCap {
		return errVNCGlobalCapReached
	}
	if len(r.byHost[hostID]) >= perHostVNCCap {
		return errVNCPerHostCapReached
	}
	return nil
}

func (r *vncSessionRegistry) insert(sess *vncSession) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.byRun) >= r.globalCap {
		return errVNCGlobalCapReached
	}
	if len(r.byHost[sess.HostID]) >= perHostVNCCap {
		return errVNCPerHostCapReached
	}
	r.byHost[sess.HostID] = append(r.byHost[sess.HostID], sess)
	r.byRun[sess.RunID] = sess
	return nil
}

func (r *vncSessionRegistry) remove(runID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess, ok := r.byRun[runID]
	if !ok {
		return
	}
	delete(r.byRun, runID)
	list := r.byHost[sess.HostID]
	for i, s := range list {
		if s.RunID == runID {
			r.byHost[sess.HostID] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(r.byHost[sess.HostID]) == 0 {
		delete(r.byHost, sess.HostID)
	}
}

func (r *vncSessionRegistry) get(runID int64) (*vncSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess, ok := r.byRun[runID]
	return sess, ok
}

// findActiveFor returns the most-recently-opened session belonging to
// this operator on this (host, host_skill). VNC reattach intentionally
// scopes to the operator: two admins both viewing the same host should
// each get their own RFB stream, since noVNC's input events go to the
// shared desktop and two simultaneous mice fighting each other is
// confusing.
func (r *vncSessionRegistry) findActiveFor(hostID string, hostSkillID int64, operatorUserID string) *vncSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	var picked *vncSession
	for _, s := range r.byHost[hostID] {
		if s.HostSkillID != hostSkillID || s.OperatorUserID != operatorUserID {
			continue
		}
		if picked == nil || s.OpenedAt.After(picked.OpenedAt) {
			picked = s
		}
	}
	return picked
}

// attachBridge installs the new bridge and returns (previous bridge,
// pre-bridge handshake snapshot). Caller is expected to evict the
// previous bridge AND replay the snapshot to the freshly-attached one
// before pumping live bytes. After the FIRST attach we stop buffering;
// subsequent re-attaches (refresh) won't get a replay — they have to
// trigger a fresh /vnc/open which mints a new run_id (the handler no
// longer dedups by host+skill+operator), so the entire RFB handshake
// re-runs cleanly.
func (r *vncSessionRegistry) attachBridge(runID int64, b *vncBridge) (*vncBridge, []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess, ok := r.byRun[runID]
	if !ok {
		return nil, nil
	}
	prev := sess.bridge
	sess.bridge = b
	var snap []byte
	if !sess.bridgeAttached && sess.handshakeBuf != nil {
		snap = sess.handshakeBuf.snapshot()
		sess.bridgeAttached = true
		// Free the buffer — we don't need it anymore.
		sess.handshakeBuf = nil
	}
	return prev, snap
}

func (r *vncSessionRegistry) detachBridge(runID int64, b *vncBridge) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess, ok := r.byRun[runID]
	if !ok || sess.bridge != b {
		return
	}
	sess.bridge = nil
}

func (r *vncSessionRegistry) forwardEvent(runID int64, ev skillEventFrame) {
	r.mu.Lock()
	sink := chan skillEventFrame(nil)
	if sess, ok := r.byRun[runID]; ok && sess.bridge != nil {
		sink = sess.bridge.eventSink
	}
	r.mu.Unlock()
	if sink == nil {
		return
	}
	select {
	case sink <- ev:
	default:
	}
}

func (r *vncSessionRegistry) forwardStdout(runID int64, chunk []byte) {
	r.mu.Lock()
	var sink chan []byte
	if sess, ok := r.byRun[runID]; ok {
		// Always-on capture until first bridge attaches — saves the
		// RFB handshake bytes from the race window between the agent
		// dialing :5900 and the browser's WS upgrade.
		if !sess.bridgeAttached && sess.handshakeBuf != nil {
			sess.handshakeBuf.write(chunk)
		}
		if sess.bridge != nil {
			sink = sess.bridge.stdoutSink
		}
	}
	r.mu.Unlock()
	if sink == nil {
		return
	}
	select {
	case sink <- chunk:
	default:
		// Backpressure: dropping frame bytes mid-stream corrupts the RFB
		// stream irrecoverably. The cleaner failure mode is to evict
		// the bridge so the browser reconnects with a fresh handshake.
		r.mu.Lock()
		if sess, ok := r.byRun[runID]; ok && sess.bridge != nil && sess.bridge.cancel != nil {
			cancel := sess.bridge.cancel
			r.mu.Unlock()
			cancel()
			return
		}
		r.mu.Unlock()
	}
}

func (r *vncSessionRegistry) snatchBridge(runID int64) *vncBridge {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess, ok := r.byRun[runID]
	if !ok {
		return nil
	}
	prev := sess.bridge
	sess.bridge = nil
	return prev
}

func (r *vncSessionRegistry) listByHost(hostID string) []vncSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.byHost[hostID]
	out := make([]vncSession, 0, len(list))
	for _, s := range list {
		out = append(out, vncSession{
			RunID:          s.RunID,
			HostID:         s.HostID,
			HostSkillID:    s.HostSkillID,
			OperatorUserID: s.OperatorUserID,
			OpenedAt:       s.OpenedAt,
			Target:         s.Target,
		})
	}
	return out
}

func (r *vncSessionRegistry) kick(runID int64) error {
	r.mu.Lock()
	sess, ok := r.byRun[runID]
	r.mu.Unlock()
	if !ok {
		return errVNCSessionNotFound
	}
	if sess.bridge != nil && sess.bridge.cancel != nil {
		sess.bridge.cancel()
	}
	return nil
}
