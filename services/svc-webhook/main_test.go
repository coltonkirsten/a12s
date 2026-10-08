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
	if !strings.HasPrefix(s, "GitHub pull_request (opened) on c/a12s by colton\nPR: Fix https://x/1\n") {
		t.Fatal(s)
	}
}
