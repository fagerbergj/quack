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
// response, independent of what quack writes back.
//
// Delivery is PACED behind Write, not dumped all at once: a plain buffered
// reader that hands back everything instantly races the connection's own
// read loop past EOF before the client has even issued Initialize.
// Releasing received[i] only once at least min(i+1, len(sent)) requests
// have been written mirrors the real protocol's shape closely enough, since
// each RPC writes its request before awaiting a response. Write only counts
// frames/errors once the live round sends more than the recording did (the
// ACP twin of replay.MissError's "extra" class).
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
