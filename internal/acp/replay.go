package acp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// replayAgentIO stands in for a real subprocess's stdin/stdout pipes when
// Options.Replay is set (proc.go's startReplay). Read serves the recorded
// AGENT→CLIENT frames verbatim, in order - the transcript IS the round's
// response, independent of what quack writes back, since it was captured as
// the agent's ACTUAL observed output for this exact recorded round.
//
// Delivery is PACED behind Write, not dumped all at once: the recording only
// preserves each direction's OWN internal order, not how sent and received
// interleaved live, and a plain buffered reader that hands back everything
// instantly races the connection's own read loop past EOF (closing the
// connection) before the client has even issued Initialize. Releasing
// received[i] only once at least min(i+1, len(sent)) requests have been
// written mirrors the real protocol's own shape closely enough: each of
// Initialize/NewSession/Prompt writes its request before awaiting a
// response, so pacing 1 release per write never blocks a call waiting on
// its OWN already-written request; once every request has been sent, the
// rest (trailing notifications, the final response) streams straight
// through with no further gate. Write only counts frames and errors once
// the live round sends more than the recording did - a live request the
// recording never answered, the ACP twin of replay.MissError's "extra"
// class. As simple as replay-strict needs: sequence playback, no live
// divergence branching (#604).
type replayAgentIO struct {
	pr *io.PipeReader
	pw *io.PipeWriter

	mu       sync.Mutex
	cond     *sync.Cond
	sentSeen int
	wantSent int
	closed   bool
}

var _ io.ReadWriteCloser = (*replayAgentIO)(nil)

func newReplayAgentIO(sent, received []json.RawMessage) *replayAgentIO {
	pr, pw := io.Pipe()
	p := &replayAgentIO{pr: pr, pw: pw, wantSent: len(sent)}
	p.cond = sync.NewCond(&p.mu)
	go p.pump(received)
	return p
}

// pump releases received's frames onto the pipe one at a time, gated by
// Write's running count - see the type doc for why. Deliberately does NOT
// close pw once done: an EOF right after the last frame would race the
// SDK's own async notification-completion bookkeeping (a real subprocess's
// stdout only EOFs when the process actually exits, not mid-response) -
// Close (called from procHandle.close, once the round is fully done) is
// the only thing that ends the pipe.
func (p *replayAgentIO) pump(received []json.RawMessage) {
	for i, l := range received {
		need := i + 1
		if need > p.wantSent {
			need = p.wantSent
		}
		p.mu.Lock()
		for p.sentSeen < need && !p.closed {
			p.cond.Wait()
		}
		closed := p.closed
		p.mu.Unlock()
		if closed {
			return
		}
		line := make([]byte, 0, len(l)+1)
		line = append(line, l...)
		line = append(line, '\n')
		if _, err := p.pw.Write(line); err != nil {
			return // reader side closed (round ended) - nothing left to feed
		}
	}
}

func (p *replayAgentIO) Read(b []byte) (int, error) { return p.pr.Read(b) }

func (p *replayAgentIO) Write(b []byte) (int, error) {
	n := bytes.Count(b, []byte("\n"))
	p.mu.Lock()
	p.sentSeen += n
	over := p.sentSeen > p.wantSent
	p.cond.Broadcast()
	p.mu.Unlock()
	if over {
		return 0, fmt.Errorf("acp: replay: live round sent more requests than the recording answered (%d recorded)", p.wantSent)
	}
	return len(b), nil
}

// Close unblocks a pump still waiting on a write that will never come (the
// round ended before every request was sent) and releases the pipe.
func (p *replayAgentIO) Close() error {
	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
	return p.pr.Close()
}
