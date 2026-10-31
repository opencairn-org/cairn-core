package main

import (
	"testing"

	"github.com/google/uuid"
)

func TestNormalizePathCollapsesContentTokens(t *testing.T) {
	if got := normalizePath("/content/abc.def"); got != "/content/:token" {
		t.Fatalf("got %q", got)
	}
	if got := normalizePath("/api/activities/" + uuid.NewString() + "/map.png"); got != "/api/activities/:id/map.png" {
		t.Fatalf("got %q", got)
	}
}
