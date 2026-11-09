package runs

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

type Error struct {
	Code        string
	ExpectedSeq int64
}

func (e *Error) Error() string { return e.Code }
func Fail(code string) error   { return &Error{Code: code} }
func Decode(b []byte, v any) error {
	if !utf8.Valid(b) || !closedJSON(b) {
		return Fail("INVALID_REQUEST")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		return Fail("INVALID_REQUEST")
	}
	var rest any
	if d.Decode(&rest) != io.EOF {
		return Fail("INVALID_REQUEST")
	}
	return nil
}
func Code(e error) string {
	var v *Error
	if errors.As(e, &v) {
		return v.Code
	}
	return "UNAVAILABLE"
}

// Encoding/json otherwise accepts duplicate keys. Duplicates must never change
// canonical evidence or replay identity.
func closedJSON(b []byte) bool {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var read func(int) bool
	read = func(depth int) bool {
		if depth > 64 {
			return false
		}
		t, e := d.Token()
		if e != nil {
			return false
		}
		delimiter, ok := t.(json.Delim)
		if !ok {
			return true
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, e := d.Token()
				if e != nil {
					return false
				}
				s, ok := key.(string)
				if !ok || seen[s] {
					return false
				}
				seen[s] = true
				if !read(depth + 1) {
					return false
				}
			}
			t, e = d.Token()
			return e == nil && t == json.Delim('}')
		case '[':
			for d.More() {
				if !read(depth + 1) {
					return false
				}
			}
			t, e = d.Token()
			return e == nil && t == json.Delim(']')
		}
		return false
	}
	if !read(0) {
		return false
	}
	_, e := d.Token()
	return e == io.EOF
}
