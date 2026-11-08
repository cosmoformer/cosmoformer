package runs

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

type Error struct {
	Code        string
	ExpectedSeq int64
}

func (e *Error) Error() string { return e.Code }
func Fail(code string) error   { return &Error{Code: code} }
func Decode(b []byte, v any) error {
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
