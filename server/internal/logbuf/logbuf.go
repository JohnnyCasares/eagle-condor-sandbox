// Package logbuf holds a run's recent output in memory so clients can tail it.
//
// Lines carry a monotonic sequence number, which is what lets a polling client
// resume exactly where it stopped without re-reading or skipping. The full log
// always exists on disk; this is only the fast path.
package logbuf

import (
	"regexp"
	"strings"
	"sync"
)

// Playwright's "line" reporter animates progress with ANSI cursor movement
// (\x1b[1A up, \x1b[2K clear-line) plus colour codes. Rendered literally in a
// browser or a JSON payload those are noise, so strip every CSI sequence.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// StripANSI removes CSI escape sequences and carriage returns.
func StripANSI(s string) string {
	return strings.ReplaceAll(ansiRE.ReplaceAllString(s, ""), "\r", "")
}

type Line struct {
	Seq  int64  `json:"seq"`
	Text string `json:"text"`
}

// Buffer is a fixed-capacity ring of recent lines plus a fan-out to live
// subscribers. Safe for concurrent use.
type Buffer struct {
	mu       sync.RWMutex
	lines    []Line
	capacity int
	nextSeq  int64
	closed   bool

	subs map[chan Line]struct{}
}

func New(capacity int) *Buffer {
	if capacity <= 0 {
		capacity = 5000
	}
	return &Buffer{
		lines:    make([]Line, 0, capacity),
		capacity: capacity,
		subs:     make(map[chan Line]struct{}),
	}
}

// Append records one line and wakes any subscribers.
func (b *Buffer) Append(text string) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	ln := Line{Seq: b.nextSeq, Text: StripANSI(text)}
	b.nextSeq++

	if len(b.lines) == b.capacity {
		copy(b.lines, b.lines[1:])
		b.lines[len(b.lines)-1] = ln
	} else {
		b.lines = append(b.lines, ln)
	}

	subs := make([]chan Line, 0, len(b.subs))
	for ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, ch := range subs {
		// Non-blocking: a subscriber that cannot keep up misses live lines
		// rather than stalling the process reading the child's stdout. It can
		// always catch up from Since().
		select {
		case ch <- ln:
		default:
		}
	}
}

// Since returns every retained line with Seq >= from, plus the next sequence
// number to ask for. limit <= 0 means no limit.
//
// If the caller has fallen behind far enough that its lines were evicted, it
// gets the oldest retained line onward; the gap is visible as a jump in Seq.
func (b *Buffer) Since(from int64, limit int) ([]Line, int64) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	out := make([]Line, 0, min(len(b.lines), max(limit, 16)))
	for _, ln := range b.lines {
		if ln.Seq < from {
			continue
		}
		out = append(out, ln)
		if limit > 0 && len(out) == limit {
			break
		}
	}

	next := b.nextSeq
	if len(out) > 0 {
		next = out[len(out)-1].Seq + 1
	} else if from > b.nextSeq {
		next = b.nextSeq
	} else {
		next = from
	}
	return out, next
}

// NextSeq is the sequence number the next appended line will carry.
func (b *Buffer) NextSeq() int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.nextSeq
}

// Subscribe returns a channel of live lines and a cancel func. Intended for the
// SSE endpoint; the polling endpoint uses Since instead.
func (b *Buffer) Subscribe(bufferSize int) (<-chan Line, func()) {
	if bufferSize <= 0 {
		bufferSize = 256
	}
	ch := make(chan Line, bufferSize)

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			if _, ok := b.subs[ch]; ok {
				delete(b.subs, ch)
				close(ch)
			}
			b.mu.Unlock()
		})
	}
}

// Close marks the buffer finished and releases every subscriber.
func (b *Buffer) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	for ch := range b.subs {
		delete(b.subs, ch)
		close(ch)
	}
	b.mu.Unlock()
}
