package blaxel

import (
	"encoding/json"
	"strings"
	"testing"
)

// Blaxel kills keepAlive processes after 600 s unless timeout is explicitly 0,
// so Forever must reach the wire as "timeout":0.
func TestForeverTimeoutIsSerialized(t *testing.T) {
	b, err := json.Marshal(processParam(ProcessRequest{Name: "p", Command: "sleep 1", KeepAlive: true, Timeout: Forever}))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"timeout":0`, `"keepAlive":true`, `"name":"p"`} {
		if !strings.Contains(s, want) {
			t.Errorf("request %s is missing %s", s, want)
		}
	}
}

func TestUnsetTimeoutIsOmitted(t *testing.T) {
	b, err := json.Marshal(processParam(ProcessRequest{Command: "true"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "timeout") || strings.Contains(string(b), "keepAlive") {
		t.Errorf("unset fields must be omitted: %s", b)
	}
}
