package hosts

// Host module Phase 2 — in-memory shell session registry.
//
// Tracks open shell sessions per (host_id) for two reasons:
//   1. Concurrency caps. Per-host hardcoded to 4, global from
//      POLAR_MAX_SHELL_SESSIONS (default 32). The 5th attempt against
//      a host or the (4N+1)th globally returns 429.
//   2. Listing / kicking. GET /api/hosts/:id/skills/shell/sessions
//      reads from here; DELETE … kicks (closes the WS + sends
//      skill.stop to agent).
//
// The registry is intentionally not persisted: a dock restart drops
// every active session (the agent-side bash dies the moment the WS
// disconnect cleanup fires in loop.go). Operators don't expect
// shell sessions to survive a dock bounce.

import (
	"errors"
	"sync"
	"time"
)

const perHostShellCap = 4

// shellBackscrollBytes is the per-run ring buffer cap (latest stdout
// bytes kept on the dock for replay on bridge reattach). 64 KiB is
// roughly the last screenful or two of typical terminal output; large
// enough to make a refresh feel continuous, small enough that 32
// concurrent runs cap at 2 MiB total.
const shellBackscrollBytes = 64 * 1024

var (
	errShellPerHostCapReached = errors.New("shell session cap reached for this host (4)")
	errShellGlobalCapReached  = errors.New("global shell session cap reached")
	errShellSessionNotFound   = errors.New("shell session not found")
)

// shellSession is the dock-side handle for one open shell. Created by
// handleHostShellOpen, persists for the run's full lifetime (NOT just
// for the lifetime of any one browser's WS). Operators can refresh
// the page or close the tab without killing the PTY — a fresh WS to
// the same run_id reattaches.
//
// operatorUserID + openedAt feed the sessions list UI; bridge points
// to the currently-attached browser bridge (nil when detached).
type shellSession struct {
	RunID          int64     `json:"run_id"`
	HostID         string    `json:"host_id"`
	HostSkillID    int64     `json:"host_skill_id"`
	OperatorUserID string    `json:"operator_user_id"`
	OpenedAt       time.Time `json:"opened_at"`

	// bridge is the live browser bridge; nil = detached. Mutated only
	// under shellSessionRegistry.mu.
	bridge *shellBridge `json:"-"`

	// backscroll keeps the last N bytes of stdout for replay when a
	// new bridge attaches (e.g. after page refresh). The supervisor
	// writes to it on every chunk; the bridge reads a snapshot at
	// attach time and replays before listening to live stream.
	backscroll *byteRing `json:"-"`
}

// shellBridge is the dock-side handle to one attached browser. The
// pointer is the identity — attach/detach use ptr equality to avoid
// a stale defer detaching a freshly-replaced bridge.
type shellBridge struct {
	cancel     func()
	eventSink  chan skillEventFrame
	stdoutSink chan []byte
}

type shellSessionRegistry struct {
	mu        sync.Mutex
	globalCap int
	byHost    map[string][]*shellSession
	byRun     map[int64]*shellSession
}

func newShellSessionRegistry(globalCap int) *shellSessionRegistry {
	return &shellSessionRegistry{
		globalCap: globalCap,
		byHost:    map[string][]*shellSession{},
		byRun:     map[int64]*shellSession{},
	}
}

// reserve checks the per-host + global caps, but does NOT yet insert
// the session — caller has to call insert() once it has the runID.
// Two-phase so we can fail-fast on the cap before any DB / WS work.
func (r *shellSessionRegistry) reserve(hostID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.byRun) >= r.globalCap {
		return errShellGlobalCapReached
	}
	if len(r.byHost[hostID]) >= perHostShellCap {
		return errShellPerHostCapReached
	}
	return nil
}

// insert finalizes a session into the registry. Caller passes a
// fully-populated *shellSession (with cancel func bound).
func (r *shellSessionRegistry) insert(sess *shellSession) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.byRun) >= r.globalCap {
		return errShellGlobalCapReached
	}
	if len(r.byHost[sess.HostID]) >= perHostShellCap {
		return errShellPerHostCapReached
	}
	r.byHost[sess.HostID] = append(r.byHost[sess.HostID], sess)
	r.byRun[sess.RunID] = sess
	return nil
}

func (r *shellSessionRegistry) remove(runID int64) {
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

func (r *shellSessionRegistry) get(runID int64) (*shellSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess, ok := r.byRun[runID]
	return sess, ok
}

// findActiveFor returns the most-recently-opened session belonging to
// this operator on this (host, host_skill). Used by /shell/open to
// dedupe: instead of dispatching a fresh skill.start every time, an
// existing run is reused so refresh / re-pick lands the operator back
// on their live PTY.
func (r *shellSessionRegistry) findActiveFor(hostID string, hostSkillID int64, operatorUserID string) *shellSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	var picked *shellSession
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

// attachBridge installs a new bridge and returns the previous one if
// any — caller invokes prev.cancel() to evict the old browser
// (reattach = last writer wins).
func (r *shellSessionRegistry) attachBridge(runID int64, b *shellBridge) *shellBridge {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess, ok := r.byRun[runID]
	if !ok {
		return nil
	}
	prev := sess.bridge
	sess.bridge = b
	return prev
}

// detachBridge clears the bridge slot iff the currently-installed
// bridge is still ours — a fresh bridge that replaced us via attach
// must not be evicted by our defer.
func (r *shellSessionRegistry) detachBridge(runID int64, b *shellBridge) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess, ok := r.byRun[runID]
	if !ok || sess.bridge != b {
		return
	}
	sess.bridge = nil
}

// forwardEvent posts to the currently-attached bridge with drop-on-full.
// Called by the supervisor; safe when no bridge is attached.
func (r *shellSessionRegistry) forwardEvent(runID int64, ev skillEventFrame) {
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

// forwardStdout posts bytes to the currently-attached bridge with
// drop-on-full. Called by the supervisor.
func (r *shellSessionRegistry) forwardStdout(runID int64, chunk []byte) {
	r.mu.Lock()
	sink := chan []byte(nil)
	if sess, ok := r.byRun[runID]; ok && sess.bridge != nil {
		sink = sess.bridge.stdoutSink
	}
	r.mu.Unlock()
	if sink == nil {
		return
	}
	select {
	case sink <- chunk:
	default:
	}
}

// snatchBridge clears + returns the current bridge. Used by the
// supervisor on terminal events to evict the browser before cleaning
// up the registry entry.
func (r *shellSessionRegistry) snatchBridge(runID int64) *shellBridge {
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

func (r *shellSessionRegistry) listByHost(hostID string) []shellSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.byHost[hostID]
	out := make([]shellSession, 0, len(list))
	for _, s := range list {
		// Copy — cancel field not exposed to callers.
		out = append(out, shellSession{
			RunID:          s.RunID,
			HostID:         s.HostID,
			HostSkillID:    s.HostSkillID,
			OperatorUserID: s.OperatorUserID,
			OpenedAt:       s.OpenedAt,
		})
	}
	return out
}

// kick cancels the attached bridge if any. The agent-side teardown
// is the explicit responsibility of the caller (handleHostShellKick
// dispatches skill.stop separately) — kick only severs the dock-side
// bridge.
func (r *shellSessionRegistry) kick(runID int64) error {
	r.mu.Lock()
	sess, ok := r.byRun[runID]
	r.mu.Unlock()
	if !ok {
		return errShellSessionNotFound
	}
	if sess.bridge != nil && sess.bridge.cancel != nil {
		sess.bridge.cancel()
	}
	return nil
}
