package evidence

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"sync"
	"unicode/utf8"
)

const WorkerBuffer = 8388608
const PreviewAttempt = 1048576
const PreviewTotal = 33554432

// Registry is shared by both pipes; each pipe has independent pending bytes.
// Registration must finish before credentials are released to a child/helper.
type Registry struct {
	mu         sync.Mutex
	terms      [][]byte
	total, max int
}

func (r *Registry) Register(values ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, total := len(r.terms), r.total
	newTerms := [][]byte{}
	for _, v := range values {
		if v == "" || !utf8.ValidString(v) {
			return ErrInvalid
		}
		duplicate := false
		for _, old := range append(r.terms, newTerms...) {
			if string(old) == v {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		n++
		total += len(v)
		if n > 4096 || total > 262144 {
			return ErrInvalid
		}
		newTerms = append(newTerms, []byte(v))
	}
	r.terms = append(r.terms, newTerms...)
	r.total = total
	for _, v := range newTerms {
		r.max = max(r.max, len(v))
	}
	return nil
}

type Pipe struct {
	registry      *Registry
	pending, mask []byte
}

func (r *Registry) Pipe() *Pipe { return &Pipe{registry: r} }
func (p *Pipe) Feed(b []byte, eof bool) []byte {
	r := p.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	p.pending = append(p.pending, b...)
	p.mask = append(p.mask, make([]byte, len(b))...)
	for _, term := range r.terms {
		for start := 0; start <= len(p.pending)-len(term); {
			i := bytes.Index(p.pending[start:], term)
			if i < 0 {
				break
			}
			i += start
			for j := i; j < i+len(term); j++ {
				p.mask[j] = 1
			}
			start = i + 1
		}
	}
	count := len(p.pending)
	if !eof {
		count = max(0, count-max(0, r.max-1))
	}
	out := append([]byte(nil), p.pending[:count]...)
	for i := range out {
		if p.mask[i] != 0 {
			out[i] = '*'
		}
	}
	p.pending = append(p.pending[:0], p.pending[count:]...)
	p.mask = append(p.mask[:0], p.mask[count:]...)
	return out
}

// Drain always consumes the pipe, including while the bounded sink drops bytes.
// Raw JSON and helper stdout never enter this function.
func Drain(src io.Reader, p *Pipe, sink func([]byte)) error {
	b := make([]byte, 32768)
	for {
		n, e := src.Read(b)
		if n > 0 {
			sink(p.Feed(b[:n], false))
		}
		if e != nil {
			sink(p.Feed(nil, true))
			if e == io.EOF {
				return nil
			}
			return e
		}
	}
}

type Gap struct {
	Reason  string `json:"reason"`
	Unknown bool   `json:"unknown_extent"`
}
type Segment struct {
	Sequence int64   `json:"sequence"`
	Start    int64   `json:"byte_start"`
	End      int64   `json:"byte_end"`
	Text     *string `json:"text,omitempty"`
	Gap      *Gap    `json:"gap,omitempty"`
}

// Queue preserves loss as an explicit ordered range, including a trailing gap.
type Queue struct {
	mu           sync.Mutex
	segments     []Segment
	bytes        int
	next, offset int64
	sealed       int64
	peeked       bool
}

func (q *Queue) Append(b []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(b) > 0 {
		n := min(len(b), int(SegmentMax/8))
		v := string(bytes.ToValidUTF8(b[:n], []byte("�")))
		s := Segment{Sequence: q.next, Start: q.offset, End: q.offset + int64(len(v))}
		q.offset = s.End
		q.next++
		if q.bytes+len(v) > WorkerBuffer {
			s.Gap = &Gap{Reason: "worker_buffer_overflow"}
			if len(q.segments) > 0 && q.segments[len(q.segments)-1].Gap != nil && (!q.peeked || q.segments[len(q.segments)-1].Sequence != q.sealed) {
				q.segments[len(q.segments)-1].End = s.End
				q.next--
				b = b[n:]
				continue
			}
		} else {
			s.End = s.Start + int64(len(v))
			q.offset = s.End
			s.Text = &v
			q.bytes += len(v)
		}
		q.segments = append(q.segments, s)
		b = b[n:]
	}
}
func (q *Queue) Take() []Segment {
	q.mu.Lock()
	defer q.mu.Unlock()
	s := q.segments
	q.segments = nil
	q.bytes = 0
	return s
}

type Cursor struct {
	Attempt string `json:"attempt_id"`
	Stream  string `json:"stream"`
	Epoch   string `json:"epoch"`
	Next    int64  `json:"next_byte"`
	Archive int64  `json:"archive_next_sequence"`
}

func (c Cursor) Encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}
func DecodeCursor(raw, attempt string) (Cursor, error) {
	var c Cursor
	if len(raw) > 1024 {
		return c, ErrInvalid
	}
	b, e := base64.RawURLEncoding.DecodeString(raw)
	if e != nil || json.Unmarshal(b, &c) != nil || c.Attempt != attempt || c.Stream != "console" || len(c.Epoch) > 64 || c.Next < 0 || c.Next > 1<<53 || c.Archive < 0 || c.Archive > 1<<53 {
		return c, ErrInvalid
	}
	return c, nil
}

type preview struct {
	epoch, agent string
	sequence     int64
	segments     []Segment
	bytes        int
}
type Previews struct {
	mu      sync.Mutex
	entries map[string]*preview
	bytes   int
	epoch   func() string
}

func NewPreviews(epoch func() string) *Previews {
	return &Previews{entries: map[string]*preview{}, epoch: epoch}
}
func (p *Previews) Append(attempt, agent string, s Segment) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if agent == "" || len(agent) > 64 || s.Sequence < 0 || s.Start < 0 || s.End < s.Start || s.Text == nil && s.Gap == nil || s.Text != nil && s.Gap != nil {
		return "", ErrInvalid
	}
	n := 0
	if s.Validate() != nil {
		return "", ErrInvalid
	}
	if s.Text != nil {
		n = len(*s.Text)
		if n > 65536 || !utf8.ValidString(*s.Text) || s.End-s.Start != int64(n) {
			return "", ErrInvalid
		}
	}
	v := p.entries[attempt]
	if v == nil {
		v = &preview{epoch: p.epoch(), agent: agent, sequence: -1}
		p.entries[attempt] = v
	}
	if v.agent == "" {
		v.agent = agent
	}
	if v.agent != agent {
		return "", ErrInvalid
	}
	if s.Sequence <= v.sequence {
		for _, old := range v.segments {
			if old.Sequence == s.Sequence {
				a, _ := json.Marshal(old)
				b, _ := json.Marshal(s)
				if bytes.Equal(a, b) {
					return v.epoch, nil
				}
			}
		}
		return "", ErrInvalid
	}
	if len(v.segments) > 0 && s.Start < v.segments[len(v.segments)-1].End {
		return "", ErrInvalid
	}
	if len(v.segments) == 0 && s.Start > 0 {
		v.segments = append(v.segments, Segment{Start: 0, End: s.Start, Gap: &Gap{"preview_loss", false}})
	}
	if len(v.segments) > 0 && s.Start > v.segments[len(v.segments)-1].End {
		v.segments = append(v.segments, Segment{Start: v.segments[len(v.segments)-1].End, End: s.Start, Gap: &Gap{"preview_loss", false}})
	}
	v.sequence = s.Sequence
	v.segments = append(v.segments, s)
	v.bytes += n
	p.bytes += n
	if v.bytes > PreviewAttempt || len(v.segments) > 1024 {
		p.bytes -= v.bytes
		delete(p.entries, attempt)
		return p.epoch(), nil
	}
	for p.bytes > PreviewTotal || len(p.entries) > 1024 {
		for id, x := range p.entries {
			p.bytes -= x.bytes
			delete(p.entries, id)
			break
		}
	}
	return v.epoch, nil
}
func (p *Previews) Snapshot(attempt string) (string, []Segment) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v := p.entries[attempt]
	if v == nil {
		if len(p.entries) >= 1024 {
			for id, x := range p.entries {
				p.bytes -= x.bytes
				delete(p.entries, id)
				break
			}
		}
		v = &preview{epoch: p.epoch(), sequence: -1}
		p.entries[attempt] = v
	}
	return v.epoch, append([]Segment(nil), v.segments...)
}

func (s Segment) Validate() error {
	if s.Sequence < 0 || s.Start < 0 || s.End < s.Start || s.End > 1<<53 || (s.Text == nil) == (s.Gap == nil) {
		return ErrInvalid
	}
	if s.Text != nil && (!utf8.ValidString(*s.Text) || s.End-s.Start != int64(len(*s.Text))) {
		return ErrInvalid
	}
	if s.Gap != nil && (len(s.Gap.Reason) == 0 || len(s.Gap.Reason) > 128) {
		return ErrInvalid
	}
	return nil
}

// Peek/Acknowledge keep unacknowledged custody in the bounded queue. Peeking
// seals a trailing gap so concurrent draining cannot extend an in-flight record.
func (q *Queue) Peek() (Segment, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.segments) == 0 {
		return Segment{}, false
	}
	q.sealed = q.segments[0].Sequence
	q.peeked = true
	return q.segments[0], true
}
func (q *Queue) Acknowledge(sequence int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.segments) == 0 || q.segments[0].Sequence != sequence {
		return ErrInvalid
	}
	if q.segments[0].Text != nil {
		q.bytes -= len(*q.segments[0].Text)
	}
	q.segments = q.segments[1:]
	return nil
}

// TextPrefix keeps byte cursors at UTF-8 boundaries. A tiny limit that cannot
// hold the next code point is rejected rather than silently replacing bytes.
func TextPrefix(text string, limit int) (string, error) {
	if !utf8.ValidString(text) || limit < 1 {
		return "", ErrInvalid
	}
	if len(text) <= limit {
		return text, nil
	}
	v := text[:limit]
	for len(v) > 0 && !utf8.ValidString(v) {
		v = v[:len(v)-1]
	}
	if len(v) == 0 {
		return "", ErrInvalid
	}
	return v, nil
}
