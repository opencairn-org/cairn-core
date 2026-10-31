package federation

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

const (
	asContext = "https://www.w3.org/ns/activitystreams"
	asPublic  = "https://www.w3.org/ns/activitystreams#Public"
)

// Instance is this server's public origin.
type Instance struct {
	Base string // no trailing slash
	Host string
}

// NewInstance parses the public base URL.
func NewInstance(publicBaseURL string) Instance {
	base := strings.TrimRight(publicBaseURL, "/")
	in := Instance{Base: base}
	if u, err := url.Parse(base); err == nil {
		in.Host = u.Host
	}
	return in
}

// ActorID is a local user's actor URL.
func (in Instance) ActorID(username string) string { return in.Base + "/users/" + username }

// Audience resolves what a viewer may see; *view.Visibility satisfies it.
type Audience interface {
	For(ctx context.Context, act *domain.Activity, viewer *domain.UserID, hasLink bool) domain.CategorySet
}

// publicCats is what an anonymous viewer may see of act; federated copies
// are public, so they never carry more.
func publicCats(ctx context.Context, aud Audience, act domain.Activity) domain.CategorySet {
	if aud == nil {
		return domain.CategorySet{}
	}
	return aud.For(ctx, &act, nil, false)
}

// BuildCreate assembles the Create + embedded object. `sport:summary` and
// `image` are what FeedItemFromObject reads on the receiving side; the map
// image is only linked when cats has it.
func BuildCreate(base, actorID, objID string, act domain.Activity, cats domain.CategorySet) map[string]any {
	sport := map[string]any{"discipline": string(act.Type)}
	if act.Summary.DistanceM != nil {
		sport["distance_m"] = *act.Summary.DistanceM
	}
	if act.MovingDuration > 0 {
		sport["moving_s"] = int(act.MovingDuration.Seconds())
	}
	if act.Summary.ElevationGainM != nil {
		sport["elevation_gain_m"] = *act.Summary.ElevationGainM
	}
	title := act.Title
	if title == "" {
		title = "Activity"
	}
	object := map[string]any{
		"id":            objID,
		"type":          "Note",
		"attributedTo":  actorID,
		"name":          title,
		"content":       act.Description,
		"url":           base + "/a/" + act.ID.String(),
		"published":     act.StartTime.UTC().Format(time.RFC3339),
		"sport:summary": sport,
	}
	if cats.Has(domain.CategoryMap) {
		object["image"] = base + "/api/activities/" + act.ID.String() + "/map.png"
	}
	return map[string]any{
		"@context":  asContext,
		"id":        objID + "#create",
		"type":      "Create",
		"actor":     actorID,
		"published": time.Now().UTC().Format(time.RFC3339),
		"to":        []string{asPublic},
		"cc":        []string{actorID + "/followers"},
		"object":    object,
	}
}

// BuildDelete is a minimal Delete of objID addressed to the followers.
func BuildDelete(actorID, objID string) map[string]any {
	return map[string]any{
		"@context": asContext,
		"id":       objID + "#delete",
		"type":     "Delete",
		"actor":    actorID,
		"to":       []string{asPublic},
		"cc":       []string{actorID + "/followers"},
		"object":   objID,
	}
}

// buildAccept accepts a Follow, embedding it verbatim.
func buildAccept(actorID, followID string, follow []byte) map[string]any {
	return map[string]any{
		"@context": asContext,
		"id":       actorID + "#accepts/" + url.PathEscape(followID),
		"type":     "Accept",
		"actor":    actorID,
		"object":   json.RawMessage(follow),
	}
}

// buildFollow follows a remote actor.
func buildFollow(actorID, remoteActorID string) map[string]any {
	return map[string]any{
		"@context": asContext,
		"id":       actorID + "#follows/" + url.PathEscape(remoteActorID),
		"type":     "Follow", "actor": actorID, "object": remoteActorID,
	}
}
