package hosts

// Per-run supervisor goroutine for shell sessions. Spawned by
// handleHostShellOpen once skill.start is dispatched; lives for the
// full lifetime of the run (NOT just the browser bridge). The
// supervisor owns the agent event channel — so when the operator
// refreshes the browser, the bridge tears down but the supervisor
// keeps observing, and the agent-side PTY stays alive until either
// a) the agent reports EventExit (idle timeout, hard cap, eof, etc.)
// b) the agent connection drops
// c) the operator explicitly kicks the run via DELETE /shell/sessions/:run_id
//
// Reattach is bridge-level: any new WS to /ws/host/:id/shell/:run_id
// gets fan-out through the supervisor's forwardEvent.

import (
	"time"
)

// shellStartSignal is what /shell/open waits on briefly before
// returning — lets the dispatcher report early skill.start failures
// (e.g. agent rejects: missing device_path) synchronously rather than
// the browser later seeing a 404 on the WS connect.
type shellStartSignal struct {
	failed bool
	reason string
}

// trackShellChannels MUST be called BEFORE dispatchSkillStart so that
// the agent's first stdout / state event has a destination channel to
// land in. Without this barrier, agent emits bytes immediately on PTY
// start, deliverSkillStdout sees no registered channel, drops silently,
// and the browser sees zero bytes through the WS. Old findActiveFor
// reattach path masked this race (no fresh dispatch); now that every
// /shell/open mints a new run, the race fires every time.
func (p *Plugin) trackShellChannels(conn *agentConn, runID int64) (chan skillEventFrame, chan []byte) {
	eventCh := conn.trackSkillPending(runID)
	stdoutCh := conn.trackSkillStdout(runID)
	return eventCh, stdoutCh
}

// startShellSupervisor spawns the supervisor goroutine for runID using
// pre-tracked channels. Caller is responsible for tracking BEFORE
// dispatching skill.start (see trackShellChannels). The returned
// channel fires once with the agent's first state (running / exit) so
// /shell/open can detect early failures.
func (p *Plugin) startShellSupervisor(conn *agentConn, runID int64, eventCh chan skillEventFrame, stdoutCh chan []byte) <-chan shellStartSignal {
	first := make(chan shellStartSignal, 1)
	go p.runShellSupervisor(conn, runID, eventCh, stdoutCh, first)
	return first
}

func (p *Plugin) runShellSupervisor(conn *agentConn, runID int64, eventCh chan skillEventFrame, stdoutCh chan []byte, first chan<- shellStartSignal) {
	defer conn.untrackSkillPending(runID)
	defer conn.untrackSkillStdout(runID)

	signalFirst := func(failed bool, reason string) {
		if first == nil {
			return
		}
		select {
		case first <- shellStartSignal{failed: failed, reason: reason}:
		default:
		}
		first = nil
	}

	cleanup := func(state, reason string) {
		// If we never signaled first, do it now — exit-before-running
		// is a failure from /open's perspective.
		signalFirst(true, reason)
		_ = p.finalizeHostSkillRun(runID, state, reason, time.Now().UTC())
		if b := p.shellSessions.snatchBridge(runID); b != nil && b.cancel != nil {
			b.cancel()
		}
		p.shellSessions.remove(runID)
	}

	for {
		select {
		case ev, ok := <-eventCh:
			if !ok {
				cleanup("ended", "agent_disconnect")
				return
			}
			p.shellSessions.forwardEvent(runID, ev)
			if ev.EventKind == "state" {
				// First state event = agent confirmed start. Tell /open
				// the run is live.
				signalFirst(false, "")
			}
			if ev.EventKind == "exit" {
				reason, _ := ev.Data["reason"].(string)
				cleanup("ended", reason)
				return
			}
		case chunk, ok := <-stdoutCh:
			if !ok {
				cleanup("ended", "agent_disconnect")
				return
			}
			// First stdout also implies a working PTY — not all skills
			// emit a state=running event before bytes start flowing.
			signalFirst(false, "")
			// Backscroll: keep the last N bytes so a reattaching
			// bridge can replay the most recent screen state.
			if sess, _ := p.shellSessions.get(runID); sess != nil {
				sess.backscroll.write(chunk)
			}
			p.shellSessions.forwardStdout(runID, chunk)
		case <-conn.close:
			cleanup("ended", "agent_disconnect")
			return
		}
	}
}
