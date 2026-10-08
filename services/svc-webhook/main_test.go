package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestSignature(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !validSignature([]byte("s3cret"), body, sig) {
		t.Fatal("good signature refused")
	}
	for _, bad := range []string{"", "sha1=" + sig[7:], "sha256=zz", sig[:len(sig)-2] + "00"} {
		if validSignature([]byte("s3cret"), body, bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	if validSignature([]byte("other"), body, sig) {
		t.Fatal("wrong secret accepted")
	}
}

func TestSummary(t *testing.T) {
	s := summarize("pull_request", []byte(`{"action":"opened","repository":{"full_name":"c/a12s"},"sender":{"login":"colton"},"pull_request":{"title":"Fix","html_url":"https://x/1"}}`))
	// What the agent needs to act on, not the wording.
	for _, want := range []string{"pull_request", "opened", "c/a12s", "colton", "Fix", "https://x/1"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q:\n%s", want, s)
		}
	}
}
