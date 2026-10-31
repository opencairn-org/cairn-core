package account

import (
	"context"
	"errors"
	"strings"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ProfilePatch carries the fields a caller wants to change; nil leaves a
// field as it is. Email is deliberately absent: an address change needs a
// verification round-trip that neither surface offers yet.
type ProfilePatch struct {
	DisplayName *string
	AvatarURL   *string
	Locale      *string
	Timezone    *string
	Units       *string
}

// ErrEmailChangeUnsupported answers a patch that names the email.
var ErrEmailChangeUnsupported = errors.New("email changes are not supported; contact an admin")

// Profiles edits the caller's own profile.
type Profiles struct {
	Users port.UserProfileStore
}

// Update validates the patch over the current row, writes it and returns the new row.
func (p *Profiles) Update(ctx context.Context, userID domain.UserID, patch ProfilePatch) (domain.User, error) {
	u, err := p.Users.GetUser(ctx, userID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.User{}, port.NewActionError(port.ActionErrorNotFound, "user not found")
		}
		return domain.User{}, internalErr("load user", err)
	}
	upd, err := applyPatch(u, patch)
	if err != nil {
		return domain.User{}, invalid(err)
	}
	if err := p.Users.UpdateProfile(ctx, userID, upd); err != nil {
		return domain.User{}, internalErr("update profile", err)
	}
	u, err = p.Users.GetUser(ctx, userID)
	if err != nil {
		return domain.User{}, internalErr("reload user", err)
	}
	return u, nil
}

// applyPatch lays the patch over the current row and validates each changed field.
func applyPatch(u domain.User, patch ProfilePatch) (port.ProfileUpdate, error) {
	upd := port.ProfileUpdate{
		DisplayName: u.DisplayName, AvatarURL: u.AvatarURL,
		Locale: u.Locale, Timezone: u.Timezone, Units: string(u.Units),
	}
	set := func(dst *string, src *string, check func(string) error) error {
		if src == nil {
			return nil
		}
		*dst = strings.TrimSpace(*src)
		return check(*dst)
	}
	steps := []error{
		set(&upd.DisplayName, patch.DisplayName, func(v string) error {
			if v == "" {
				return domain.ErrDisplayNameRequired
			}
			return nil
		}),
		set(&upd.AvatarURL, patch.AvatarURL, domain.ValidateAvatarURL),
		set(&upd.Locale, patch.Locale, domain.ValidateLocale),
		set(&upd.Timezone, patch.Timezone, domain.ValidateTimezone),
		set(&upd.Units, patch.Units, func(v string) error {
			if !domain.UserUnits(v).Valid() {
				return errors.New("units must be metric or imperial")
			}
			return nil
		}),
	}
	for _, err := range steps {
		if err != nil {
			return port.ProfileUpdate{}, err
		}
	}
	return upd, nil
}
