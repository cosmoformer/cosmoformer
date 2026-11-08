package worker

import (
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"strings"
	"testing"
)

func TestCaptureSeparatesProtectedOutput(t *testing.T) {
	for _, kind := range []string{"console", "raw-json", "credential-helper"} {
		r := &evidence.Registry{}
		r.Register("secret")
		c := NewLogCapture(r)
		if e := c.Drain(strings.NewReader("secret stdout"), strings.NewReader("secret stderr"), kind); e != nil {
			t.Fatal(e)
		}
		var text string
		for _, s := range c.Queue.Take() {
			if s.Text != nil {
				text += *s.Text
			}
		}
		if strings.Contains(text, "secret") || kind != "console" && strings.Contains(text, "stdout") || kind == "credential-helper" && text != "" {
			t.Fatal(kind, text)
		}
	}
}
