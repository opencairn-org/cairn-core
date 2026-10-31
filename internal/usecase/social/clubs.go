package social

import (
	"context"
	"regexp"
	"strings"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// Clubs creates clubs and manages the caller's membership.
type Clubs struct {
	Repo port.ClubRepo
}

// NewClub is a create request as the caller typed it.
type NewClub struct {
	Name        string
	Description string
	IsPublic    *bool // nil: public
}

var clubSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

const clubSlugMax = 60

// Slugify turns a club name into a URL-safe slug.
func Slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = clubSlugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > clubSlugMax {
		s = s[:clubSlugMax]
	}
	return s
}

// normalize trims and caps the request into a club owned by me.
func (n NewClub) normalize(me domain.UserID) (domain.Club, error) {
	name := strings.TrimSpace(n.Name)
	if name == "" {
		return domain.Club{}, port.NewActionError(port.ActionErrorInvalid, "name required")
	}
	if len(name) > domain.MaxClubNameLength {
		name = name[:domain.MaxClubNameLength]
	}
	slug := Slugify(name)
	if slug == "" {
		return domain.Club{}, port.NewActionError(port.ActionErrorInvalid, "name must contain letters or digits")
	}
	desc := strings.TrimSpace(n.Description)
	if len(desc) > domain.MaxClubDescriptionLength {
		desc = desc[:domain.MaxClubDescriptionLength]
	}
	public := true
	if n.IsPublic != nil {
		public = *n.IsPublic
	}
	return domain.Club{Slug: slug, Name: name, Description: desc, OwnerID: me, IsPublic: public}, nil
}

// Create stores a club owned by me and returns its id and final slug.
func (c *Clubs) Create(ctx context.Context, me domain.UserID, req NewClub) (domain.ClubID, string, error) {
	club, err := req.normalize(me)
	if err != nil {
		return domain.ClubID{}, "", err
	}
	// Disambiguate slug collisions by suffixing the owner's short id.
	if _, err := c.Repo.GetBySlug(ctx, club.Slug); err == nil {
		club.Slug += "-" + me.String()[:8]
	}
	id, err := c.Repo.Create(ctx, club)
	if err != nil {
		return domain.ClubID{}, "", err
	}
	return id, club.Slug, nil
}

// club resolves a slug; any lookup failure reads as not found.
func (c *Clubs) club(ctx context.Context, slug string) (domain.Club, error) {
	club, err := c.Repo.GetBySlug(ctx, slug)
	if err != nil {
		return domain.Club{}, port.NewActionError(port.ActionErrorNotFound, "club not found")
	}
	return club, nil
}

// Join adds me as a member of a public club.
func (c *Clubs) Join(ctx context.Context, me domain.UserID, slug string) error {
	club, err := c.club(ctx, slug)
	if err != nil {
		return err
	}
	if !club.IsPublic {
		return port.NewActionError(port.ActionErrorForbidden, "this club is invite-only")
	}
	return c.Repo.AddMember(ctx, club.ID, me, domain.ClubRoleMember)
}

// Leave removes me from a club I do not own.
func (c *Clubs) Leave(ctx context.Context, me domain.UserID, slug string) error {
	club, err := c.club(ctx, slug)
	if err != nil {
		return err
	}
	if club.OwnerID == me {
		return port.NewActionError(port.ActionErrorInvalid, "the owner cannot leave their own club")
	}
	return c.Repo.RemoveMember(ctx, club.ID, me)
}
