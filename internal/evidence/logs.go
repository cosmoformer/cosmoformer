package evidence

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"
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
	sort.SliceStable(r.terms, func(i, j int) bool { return len(r.terms[i]) > len(r.terms[j]) })
	r.total = total
	for _, v := range newTerms {
		r.max = max(r.max, len(v))
	}
	return nil
}

// Contains checks decoded strings and canonical scalar bytes, including keys
// and identifiers. It never returns the matched value to its caller.
func (r *Registry) Contains(value string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, term := range r.terms {
		if strings.Contains(value, string(term)) {
			return true
		}
	}
	return false
}

// SecretTerms is shared by log preparation, analysis and independent platform
// validation. JSON TF_VAR inputs additionally protect decoded scalar leaves.
func SecretTerms(value string, jsonVariable bool) ([]string, error) {
	if value == "" || len(value) > 16384 || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return nil, ErrInvalid
	}
	terms := []string{value}
	if !jsonVariable || !json.Valid([]byte(value)) {
		return terms, nil
	}
	d := json.NewDecoder(strings.NewReader(value))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil {
		return nil, ErrInvalid
	}
	seen := map[string]bool{value: true}
	total := len(value)
	add := func(term string) error {
		if seen[term] {
			return nil
		}
		if len(terms) >= 4096 || total+len(term) > 262144 {
			return ErrInvalid
		}
		seen[term] = true
		terms = append(terms, term)
		total += len(term)
		return nil
	}
	var leaves func(any, int) error
	leaves = func(v any, depth int) error {
		if depth > 96 {
			return ErrInvalid
		}
		switch x := v.(type) {
		case string:
			if x != "" {
				if e := add(x); e != nil {
					return e
				}
			}
		case json.Number:
			n, e := CanonicalNumber(string(x))
			if e != nil {
				return e
			}
			if e := add(string(x)); e != nil {
				return e
			}
			if e := add(n); e != nil {
				return e
			}
		case bool:
			if e := add(strconv.FormatBool(x)); e != nil {
				return e
			}
		case []any:
			for _, child := range x {
				if e := leaves(child, depth+1); e != nil {
					return e
				}
			}
		case map[string]any:
			for _, child := range x {
				if e := leaves(child, depth+1); e != nil {
					return e
				}
			}
		}
		return nil
	}
	if e := leaves(v, 0); e != nil {
		return nil, e
	}
	return terms, nil
}

// CanonicalNumber expands JSON decimals without float conversion. Bounds apply
// before allocation, including exponent expansion; overflow never truncates.
func CanonicalNumber(value string) (string, error) {
	if len(value) > 16384 || strings.TrimSpace(value) != value || !json.Valid([]byte(value)) || value == "" || !strings.ContainsAny(value[:1], "-0123456789") {
		return "", ErrInvalid
	}
	negative := strings.HasPrefix(value, "-")
	s := strings.TrimPrefix(value, "-")
	exponent := 0
	if at := strings.IndexAny(s, "eE"); at >= 0 {
		var e error
		exponent, e = strconv.Atoi(s[at+1:])
		if e != nil || exponent < -16384 || exponent > 16384 {
			return "", ErrInvalid
		}
		s = s[:at]
	}
	decimal := strings.IndexByte(s, '.')
	if decimal < 0 {
		decimal = len(s)
	}
	digits := strings.ReplaceAll(s, ".", "")
	position := decimal + exponent
	if position < 0 {
		digits = "0." + strings.Repeat("0", -position) + digits
	} else if position >= len(digits) {
		digits += strings.Repeat("0", position-len(digits))
	} else {
		digits = digits[:position] + "." + digits[position:]
	}
	halves := strings.SplitN(digits, ".", 2)
	integer := strings.TrimLeft(halves[0], "0")
	if integer == "" {
		integer = "0"
	}
	result := integer
	if len(halves) == 2 {
		if fraction := strings.TrimRight(halves[1], "0"); fraction != "" {
			result += "." + fraction
		}
	}
	if result != "0" && negative {
		result = "-" + result
	}
	if len(result) > 16384 {
		return "", ErrInvalid
	}
	return result, nil
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
	// Keep a partial character in this pipe, alongside the secret overlap,
	// before the masked stdout/stderr chunks can be combined.
	if !eof {
		for i := 0; i < len(out); {
			if !utf8.FullRune(out[i:]) {
				count = i
				out = out[:i]
				break
			}
			_, n := utf8.DecodeRune(out[i:])
			i += n
		}
	}
	out = bytes.ToValidUTF8(out, []byte("\uFFFD"))
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

// Queue reserves fixed metadata and coalesces small reads into bounded blocks.
// Leave room inside WorkerBuffer for the ring, a Peek snapshot and UTF-8 work.
const queueBlockBytes = 65536
const queuePayloadBytes = WorkerBuffer - 2*queueBlockBytes
const queueEntries = 256

type queuedSegment struct {
	sequence, start, end int64
	data                 []byte
	gap                  bool
	sealed               bool
}
type Queue struct {
	mu                 sync.Mutex
	entries            [queueEntries]queuedSegment
	head, count, bytes int
	next, offset       int64
}

func (q *Queue) tail() *queuedSegment {
	if q.count == 0 {
		return nil
	}
	return &q.entries[(q.head+q.count-1)%queueEntries]
}
func (q *Queue) addGap(n int) {
	end := q.offset + int64(n)
	if tail := q.tail(); tail != nil && tail.gap && !tail.sealed {
		tail.end = end
	} else {
		q.entries[(q.head+q.count)%queueEntries] = queuedSegment{sequence: q.next, start: q.offset, end: end, gap: true}
		q.count++
		q.next++
	}
	q.offset = end
}
func (q *Queue) Append(b []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(b) > 0 {
		n := min(len(b), queueBlockBytes)
		// Pipe emits complete UTF-8 characters; preserve those at block boundaries.
		if n < len(b) {
			for n > 0 && !utf8.RuneStart(b[n]) {
				n--
			}
			if n == 0 {
				n = min(len(b), queueBlockBytes)
			}
		}
		part := bytes.ToValidUTF8(b[:n], []byte("\uFFFD"))
		q.appendText(part)
		b = b[n:]
	}
}
func (q *Queue) appendText(b []byte) {
	for len(b) > 0 {
		tail := q.tail()
		if tail == nil || tail.gap || tail.sealed || len(tail.data) == queueBlockBytes {
			if q.bytes+queueBlockBytes > queuePayloadBytes || q.count >= queueEntries-1 {
				q.addGap(len(b))
				return
			}
			q.entries[(q.head+q.count)%queueEntries] = queuedSegment{sequence: q.next, start: q.offset, end: q.offset, data: make([]byte, 0, queueBlockBytes)}
			q.count++
			q.next++
			q.bytes += queueBlockBytes
			tail = q.tail()
		}
		n := min(len(b), queueBlockBytes-len(tail.data))
		if n < len(b) {
			for n > 0 && !utf8.RuneStart(b[n]) {
				n--
			}
		}
		if n == 0 {
			tail.sealed = true
			continue
		}
		tail.data = append(tail.data, b[:n]...)
		q.offset += int64(n)
		tail.end = q.offset
		b = b[n:]
	}
}
func entrySegment(v *queuedSegment) Segment {
	s := Segment{Sequence: v.sequence, Start: v.start, End: v.end}
	if v.gap {
		s.Gap = &Gap{Reason: "worker_buffer_overflow"}
	} else {
		text := string(v.data)
		s.Text = &text
	}
	return s
}
func (q *Queue) Take() []Segment {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Segment, 0, q.count)
	for q.count > 0 {
		out = append(out, entrySegment(&q.entries[q.head]))
		q.removeHead()
	}
	return out
}
func (q *Queue) removeHead() {
	v := &q.entries[q.head]
	q.bytes -= cap(v.data)
	*v = queuedSegment{}
	q.head = (q.head + 1) % queueEntries
	q.count--
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

// Peek seals the exact in-flight segment. Concurrent appends cannot alter its
// bytes or gap range, so response-loss retries retain the same custody identity.
func (q *Queue) Peek() (Segment, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.count == 0 {
		return Segment{}, false
	}
	q.entries[q.head].sealed = true
	return entrySegment(&q.entries[q.head]), true
}
func (q *Queue) Acknowledge(sequence int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.count == 0 || q.entries[q.head].sequence != sequence {
		return ErrInvalid
	}
	q.removeHead()
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
