// Package federation is ActivityPub (docs/federation-design.md): local
// actors and their outbox, the inbox, outbound Follow/Accept, and
// publishing public activities to remote followers over the durable
// delivery queue. HTTP lives in the adapters; this package holds the rules.
//
// Gated twice: the instance flag and the per-user opt-in. A non-opted-in
// user is indistinguishable from "no such user".
package federation

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// Activity is the minimal envelope of an inbound ActivityPub activity.
type Activity struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Actor  string          `json:"actor"`
	Object json.RawMessage `json:"object"`
}

// ObjectID is the object as a URL: a bare string or an embedded object's id.
func (a Activity) ObjectID() string {
	var s string
	if json.Unmarshal(a.Object, &s) == nil && s != "" {
		return s
	}
	var o struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(a.Object, &o)
	return o.ID
}

// ObjectType is an embedded object's type, "" for a bare id.
func (a Activity) ObjectType() string {
	var o struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(a.Object, &o)
	return o.Type
}

// UndoneObject is the inner activity's object id when an Undo wraps an
// embedded activity of wantType, "" otherwise.
func (a Activity) UndoneObject(wantType string) string {
	var o struct {
		Type   string          `json:"type"`
		Object json.RawMessage `json:"object"`
	}
	if json.Unmarshal(a.Object, &o) != nil || o.Type != wantType {
		return ""
	}
	var s string
	if json.Unmarshal(o.Object, &s) == nil && s != "" {
		return s
	}
	var inner struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(o.Object, &inner)
	return inner.ID
}

// UndoneFollowTarget is the followee of an Undo{Follow}.
func (a Activity) UndoneFollowTarget() string { return a.UndoneObject("Follow") }

// LocalActivityID extracts the id from .../activities/{uuid}. The host must
// be expectHost, so a foreign URL embedding a local UUID is rejected.
func LocalActivityID(raw, expectHost string) (domain.ActivityID, bool) {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Host, expectHost) {
		return domain.ActivityID{}, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "activities" {
			if id, err := domain.ParseUUID[domain.ActivityID](parts[i+1]); err == nil {
				return id, true
			}
		}
	}
	return domain.ActivityID{}, false
}

// HostOf is the URL's host, "" when unparsable.
func HostOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return u.Host
	}
	return ""
}

// ParsePublicKeyPEM parses an actor's RSA public key.
func ParsePublicKeyPEM(p string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(p))
	if block == nil {
		return nil, errors.New("invalid public key PEM")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	rk, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("public key is not RSA")
	}
	return rk, nil
}

var htmlTag = regexp.MustCompile(`<[^>]*>`)

// FeedItemFromObject parses a Create's object into a home-feed item; false
// when it has no id.
func FeedItemFromObject(recipient domain.UserID, actorID string, obj json.RawMessage) (domain.FederatedFeedItem, bool) {
	var o struct {
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Content   string          `json:"content"`
		URL       string          `json:"url"`
		Published string          `json:"published"`
		Image     json.RawMessage `json:"image"`
		Sport     struct {
			Discipline     string  `json:"discipline"`
			DistanceM      float64 `json:"distance_m"`
			MovingS        int     `json:"moving_s"`
			ElevationGainM float64 `json:"elevation_gain_m"`
		} `json:"sport:summary"`
	}
	if err := json.Unmarshal(obj, &o); err != nil || o.ID == "" {
		return domain.FederatedFeedItem{}, false
	}
	published := time.Now().UTC()
	if t, err := time.Parse(time.RFC3339, o.Published); err == nil {
		published = t.UTC()
	}
	it := domain.FederatedFeedItem{
		RecipientID: recipient, ActorID: actorID, ActivityAPID: o.ID, Published: published,
		Name: o.Name, Summary: strings.TrimSpace(htmlTag.ReplaceAllString(o.Content, " ")),
		URL: o.URL, ImageURL: imageURL(o.Image), Sport: o.Sport.Discipline,
	}
	if o.Sport.DistanceM > 0 {
		d := o.Sport.DistanceM
		it.DistanceM = &d
	}
	if o.Sport.MovingS > 0 {
		s := o.Sport.MovingS
		it.DurationS = &s
	}
	if o.Sport.ElevationGainM > 0 {
		e := o.Sport.ElevationGainM
		it.ElevationM = &e
	}
	return it, true
}

// RemoteComment parses a Create's object as a reply Note; false when it
// isn't a reply or has no id/content. body is HTML-stripped.
func RemoteComment(obj json.RawMessage) (noteID, inReplyTo, body string, ok bool) {
	var o struct {
		ID        string `json:"id"`
		InReplyTo string `json:"inReplyTo"`
		Content   string `json:"content"`
	}
	if err := json.Unmarshal(obj, &o); err != nil || o.ID == "" || o.InReplyTo == "" {
		return "", "", "", false
	}
	body = strings.TrimSpace(htmlTag.ReplaceAllString(o.Content, " "))
	if body == "" {
		return "", "", "", false
	}
	return o.ID, o.InReplyTo, body, true
}

// imageURL accepts a string or an {url:""} object.
func imageURL(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return s
	}
	var o struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(raw, &o)
	return o.URL
}
