package hosts

// Per-run supervisor goroutine for VNC sessions. Spawned by
// handleHostVNCOpen once skill.start is dispatched; lives for the full
// lifetime of the run. Mirrors host_shell_supervisor.go.
//
// Why a supervisor (separate from the WS bridge):
//   The agent's skill.event + stdout channels are pulled by the
//   supervisor exclusively. If we coupled them to the WS handler,
//   refreshing the browser would race against the agent stream and
//   either lose bytes or double-receive. Decoupling lets the WS bridge
//   be a thin fan-out: attach → receive forwarded chunks; detach →
//   bytes keep flowing into the (now-empty) sink → drop on backpressure
//   until a fresh bridge attaches.

import (
	"log"
	"time"
)

type vncStartSignal struct {
	failed bool
	reason string
}

// trackVNCChannels — same barrier pattern as trackShellChannels. Track
// before dispatch so the agent's early TCP-relay bytes have a destination.
func (p *Plugin) trackVNCChannels(conn *agentConn, runID int64) (chan skillEventFrame, chan []byte) {
	eventCh := conn.trackSkillPending(runID)
	stdoutCh := conn.trackSkillStdout(runID)
	return eventCh, stdoutCh
}

func (p *Plugin) startVNCSupervisor(conn *agentConn, runID int64, eventCh chan skillEventFrame, stdoutCh chan []byte) <-chan vncStartSignal {
	first := make(chan vncStartSignal, 1)
	go p.runVNCSupervisor(conn, runID, eventCh, stdoutCh, first)
	return first
}

func (p *Plugin) runVNCSupervisor(conn *agentConn, runID int64, eventCh chan skillEventFrame, stdoutCh chan []byte, first chan<- vncStartSignal) {
	log.Printf("[vnc-sup] run=%d supervisor START", runID)
	defer conn.untrackSkillPending(runID)
	defer conn.untrackSkillStdout(runID)
	defer log.Printf("[vnc-sup] run=%d supervisor EXIT — channels untracked", runID)

	signalFirst := func(failed bool, reason string) {
		if first == nil {
			return
		}
		log.Printf("[vnc-sup] run=%d signalFirst failed=%v reason=%s", runID, failed, reason)
		select {
		case first <- vncStartSignal{failed: failed, reason: reason}:
		default:
		}
		first = nil
	}

	cleanup := func(state, reason string) {
		log.Printf("[vnc-sup] run=%d cleanup state=%s reason=%s", runID, state, reason)
		signalFirst(true, reason)
		_ = p.finalizeHostSkillRun(runID, state, reason, time.Now().UTC())
		if b := p.vncSessions.snatchBridge(runID); b != nil && b.cancel != nil {
			b.cancel()
		}
		p.vncSessions.remove(runID)
	}

	var totalStdoutBytes int64
	var stdoutChunks int
	for {
		select {
		case ev, ok := <-eventCh:
			if !ok {
				log.Printf("[vnc-sup] run=%d eventCh closed — agent disconnect", runID)
				cleanup("ended", "agent_disconnect")
				return
			}
			log.Printf("[vnc-sup] run=%d event kind=%s data=%v", runID, ev.EventKind, ev.Data)
			p.vncSessions.forwardEvent(runID, ev)
			if ev.EventKind == "state" {
				signalFirst(false, "")
			}
			if ev.EventKind == "exit" {
				reason, _ := ev.Data["reason"].(string)
				cleanup("ended", reason)
				return
			}
		case chunk, ok := <-stdoutCh:
			if !ok {
				log.Printf("[vnc-sup] run=%d stdoutCh closed — agent disconnect (after %d chunks / %d bytes)", runID, stdoutChunks, totalStdoutBytes)
				cleanup("ended", "agent_disconnect")
				return
			}
			stdoutChunks++
			totalStdoutBytes += int64(len(chunk))
			if stdoutChunks <= 3 || stdoutChunks%50 == 0 {
				log.Printf("[vnc-sup] run=%d stdout chunk#%d size=%d total=%d", runID, stdoutChunks, len(chunk), totalStdoutBytes)
			}
			signalFirst(false, "")
			p.vncSessions.forwardStdout(runID, chunk)
		case <-conn.close:
			log.Printf("[vnc-sup] run=%d conn.close — agent WS dropped", runID)
			cleanup("ended", "agent_disconnect")
			return
		}
	}
}
