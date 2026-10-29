package useradmin

import (
	"context"
	"fmt"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// UserSummary is one admin list row: the user with its footprint.
type UserSummary struct {
	User      domain.User
	Footprint port.UserFootprint
}

// Page lists users with their footprints; shared by GET /api/admin/users
// and AdminService.ListUsers. A footprint failure leaves zero counts.
func (u *Users) Page(ctx context.Context, cursor string, size int) ([]UserSummary, string, error) {
	users, next, err := u.Users.ListUsers(ctx, port.UserListPage{Cursor: cursor, PageSize: size})
	if err != nil {
		return nil, "", port.NewActionError(port.ActionErrorInternal, fmt.Sprintf("list users: %v", err))
	}
	ids := make([]domain.UserID, 0, len(users))
	for _, usr := range users {
		ids = append(ids, usr.ID)
	}
	footprints, _ := u.Footprints(ctx, ids)
	out := make([]UserSummary, 0, len(users))
	for _, usr := range users {
		out = append(out, UserSummary{User: usr, Footprint: footprints[usr.ID]})
	}
	return out, next, nil
}
