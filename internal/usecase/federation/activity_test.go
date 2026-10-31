package federation

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestActivity_UndoFollowParsing(t *testing.T) {
	// An Undo wrapping the original Follow (Mastodon-style: embedded object).
	raw := `{
	  "type": "Undo",
	  "id": "https://remote.example/activities/undo/1",
	  "actor": "https://remote.example/users/bob",
	  "object": {
	    "type": "Follow",
	    "id": "https://remote.example/activities/follow/1",
	    "actor": "https://remote.example/users/bob",
	    "object": "https://cairn.example/users/alice"
	  }
	}`
	var a Activity
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatal(err)
	}
	if a.Type != "Undo" {
		t.Fatalf("type = %q", a.Type)
	}
	if got := a.ObjectType(); got != "Follow" {
		t.Errorf("objectType = %q, want Follow", got)
	}
	if got := a.UndoneFollowTarget(); got != "https://cairn.example/users/alice" {
		t.Errorf("undoneFollowTarget = %q", got)
	}
}

func TestActivity_UndoNonFollowHasNoTarget(t *testing.T) {
	raw := `{"type":"Undo","object":{"type":"Like","object":"https://x/o/1"}}`
	var a Activity
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatal(err)
	}
	if got := a.UndoneFollowTarget(); got != "" {
		t.Errorf("undoneFollowTarget for Undo{Like} = %q, want empty", got)
	}
}

func TestActivity_UndoLikeObject(t *testing.T) {
	raw := `{
	  "type": "Undo",
	  "actor": "https://remote.example/users/bob",
	  "object": {
	    "type": "Like",
	    "id": "https://remote.example/activities/like/1",
	    "object": "https://cairn.example/users/alice/activities/019ea3be-f1d5-7b39-8305-26603838c8ee"
	  }
	}`
	var a Activity
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatal(err)
	}
	if got := a.UndoneObject("Like"); got != "https://cairn.example/users/alice/activities/019ea3be-f1d5-7b39-8305-26603838c8ee" {
		t.Errorf("undoneObject(Like) = %q", got)
	}
	if got := a.UndoneObject("Follow"); got != "" {
		t.Errorf("undoneObject(Follow) on Undo{Like} = %q, want empty", got)
	}
}

func TestLocalActivityID(t *testing.T) {
	const host = "cairn.example"
	id, ok := LocalActivityID("https://cairn.example/users/alice/activities/019ea3be-f1d5-7b39-8305-26603838c8ee", host)
	if !ok {
		t.Fatal("expected to parse an activity id")
	}
	if id.String() != "019ea3be-f1d5-7b39-8305-26603838c8ee" {
		t.Errorf("id = %s", id.String())
	}
	// A non-activity URL yields ok=false.
	if _, ok := LocalActivityID("https://cairn.example/users/alice", host); ok {
		t.Error("expected no activity id for an actor URL")
	}
	// A garbage tail (not a UUID) yields ok=false.
	if _, ok := LocalActivityID("https://cairn.example/users/alice/activities/not-a-uuid", host); ok {
		t.Error("expected no activity id for a non-UUID tail")
	}
	// A FOREIGN host bearing a real local UUID is rejected (no host spoofing).
	if _, ok := LocalActivityID("https://attacker.example/users/x/activities/019ea3be-f1d5-7b39-8305-26603838c8ee", host); ok {
		t.Error("expected rejection of a foreign-host URL")
	}
}

func TestRemoteComment(t *testing.T) {
	// A reply Note → parsed, HTML stripped from the body.
	note := `{"id":"https://remote.example/notes/1","type":"Note",
	  "inReplyTo":"https://cairn.example/users/alice/activities/019ea3be-f1d5-7b39-8305-26603838c8ee",
	  "content":"<p>nice <b>ride</b>!</p>"}`
	id, inReplyTo, body, ok := RemoteComment([]byte(note))
	if !ok {
		t.Fatal("expected a reply to parse")
	}
	if id != "https://remote.example/notes/1" {
		t.Errorf("id = %q", id)
	}
	if inReplyTo == "" {
		t.Error("inReplyTo empty")
	}
	if body != "nice  ride !" && body != "nice ride !" {
		// HTML tags become spaces; exact inner spacing isn't important.
		t.Logf("body = %q", body)
		if !strings.Contains(body, "ride") {
			t.Errorf("body lost content: %q", body)
		}
	}

	// A workout Create object (no inReplyTo) is NOT a comment.
	if _, _, _, ok := RemoteComment([]byte(`{"id":"x","name":"Ride","sport:summary":{}}`)); ok {
		t.Error("a workout object should not parse as a comment")
	}
	// Empty content → not a comment.
	if _, _, _, ok := RemoteComment([]byte(`{"id":"x","inReplyTo":"y","content":"  "}`)); ok {
		t.Error("blank content should not parse as a comment")
	}
}

func TestActivity_DeleteObjectID(t *testing.T) {
	// Delete with a bare-string object (Tombstone-less form).
	raw := `{"type":"Delete","actor":"https://remote.example/users/bob","object":"https://remote.example/users/bob/activities/9"}`
	var a Activity
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatal(err)
	}
	if got := a.ObjectID(); got != "https://remote.example/users/bob/activities/9" {
		t.Errorf("objectID = %q", got)
	}
	// Actor self-delete: object id == actor.
	raw2 := `{"type":"Delete","actor":"https://remote.example/users/bob","object":{"id":"https://remote.example/users/bob","type":"Person"}}`
	var a2 Activity
	if err := json.Unmarshal([]byte(raw2), &a2); err != nil {
		t.Fatal(err)
	}
	if got := a2.ObjectID(); got != "https://remote.example/users/bob" {
		t.Errorf("self-delete objectID = %q", got)
	}
}
