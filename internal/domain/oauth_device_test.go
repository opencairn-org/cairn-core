package domain

import (
	"strings"
	"testing"
	"time"
)

func TestUserCode(t *testing.T) {
	code, err := NewUserCode()
	if err != nil || len(code) != UserCodeLength || strings.Trim(code, "0123456789") != "" {
		t.Fatalf("new code %q %v", code, err)
	}
	if DisplayUserCode("12345678") != "1234-5678" {
		t.Fatalf("display: %q", DisplayUserCode("12345678"))
	}
	for in, want := range map[string]string{"1234-5678": "12345678", " 1234 5678 ": "12345678", "12345678": "12345678"} {
		got, err := NormalizeUserCode(in)
		if err != nil || got != want {
			t.Fatalf("normalize %q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "1234-567", "1234-56789", "ABCD-EFGH", "1234_5678"} {
		if _, err := NormalizeUserCode(bad); err == nil {
			t.Fatalf("normalize %q accepted", bad)
		}
	}
}

func TestCapScopes(t *testing.T) {
	req := []string{ScopeActivitiesRead, ScopeActivitiesWrite}
	if got := CapScopes(req, nil); len(got) != 2 {
		t.Fatalf("unrestricted: %v", got)
	}
	if got := CapScopes(req, []string{ScopeActivitiesRead}); len(got) != 1 || got[0] != ScopeActivitiesRead {
		t.Fatalf("capped: %v", got)
	}
	if got := CapScopes(req, []string{}); len(got) != 0 {
		t.Fatalf("empty approver: %v", got)
	}
}

func TestDeviceCodeLive(t *testing.T) {
	now := time.Now()
	d := OAuthDeviceCode{ExpiresAt: now.Add(time.Minute)}
	if !d.IsLiveAt(now) || d.IsLiveAt(now.Add(2*time.Minute)) {
		t.Fatal("expiry")
	}
	d.ConsumedAt = &now
	if d.IsLiveAt(now) {
		t.Fatal("consumed")
	}
}
