package main

import (
	"strings"
	"testing"
)

func TestCheckCredentialShape(t *testing.T) {
	good := "sk-ant-oat01-" + strings.Repeat("a", 95)
	for _, tc := range []struct {
		name, value, want string
	}{
		{"anthropic", good, ""},
		{"anthropic", "sk-ant-api03-" + strings.Repeat("b", 95), ""},
		{"anthropic", good[:60] + "\n" + good[60:], "line breaks"},
		{"anthropic", good[:60] + " " + good[60:], "spaces"},
		{"anthropic", good[:70], "cut off"},
		{"anthropic", "ghp_" + strings.Repeat("c", 100), "not sk-ant-oat01-"},
		{"stripe", "anything at all", ""},
	} {
		err := checkCredentialShape(tc.name, []byte(tc.value))
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s %q: unexpected %v", tc.name, tc.value[:8], err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "fix:") {
			t.Errorf("%s %q: got %v, want %q and a fix line", tc.name, tc.value[:8], err, tc.want)
		}
	}
}
