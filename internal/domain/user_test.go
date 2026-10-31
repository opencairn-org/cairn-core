package domain

import (
	"errors"
	"testing"
)

func TestNormaliseEmail(t *testing.T) {
	got, err := NormaliseEmail("  Alice@Example.COM ")
	if err != nil || got != "alice@example.com" {
		t.Fatalf("NormaliseEmail = %q, %v", got, err)
	}
	if _, err := NormaliseEmail("  "); !errors.Is(err, ErrEmailRequired) {
		t.Errorf("blank: %v, want ErrEmailRequired", err)
	}
	for _, bad := range []string{
		"nope",
		"bob <bob@example.com>",
		"a@example.com, b@example.com",
		"a@example.com\r\nbcc: evil@example.com",
	} {
		if _, err := NormaliseEmail(bad); !errors.Is(err, ErrEmailInvalid) {
			t.Errorf("NormaliseEmail(%q) = %v, want ErrEmailInvalid", bad, err)
		}
	}
}

func TestNormaliseUsername(t *testing.T) {
	cases := map[string]error{
		" Alice.B_c-1 ":                     nil,
		"":                                  ErrUsernameRequired,
		"ab":                                ErrUsernameTooShort,
		"abcdefghijklmnopqrstuvwxyz0123456": ErrUsernameTooLong,
		"bad name":                          ErrUsernameInvalidChars,
		"bad@name":                          ErrUsernameInvalidChars,
	}
	for in, want := range cases {
		got, err := NormaliseUsername(in)
		if !errors.Is(err, want) {
			t.Errorf("NormaliseUsername(%q) err = %v, want %v", in, err, want)
		}
		if want == nil && got != "alice.b_c-1" {
			t.Errorf("NormaliseUsername(%q) = %q", in, got)
		}
	}
}

func TestValidateUserShape(t *testing.T) {
	ok := User{
		Username: "alice", Email: "alice@example.com", DisplayName: "Alice",
		Role: UserRoleUser, Status: UserStatusActive, Units: UserUnitsMetric,
	}
	if err := ValidateUserShape(ok); err != nil {
		t.Fatalf("valid user rejected: %v", err)
	}
	mut := func(f func(*User)) User { u := ok; f(&u); return u }
	for name, u := range map[string]User{
		"username": mut(func(u *User) { u.Username = "x" }),
		"email":    mut(func(u *User) { u.Email = "x" }),
		"display":  mut(func(u *User) { u.DisplayName = " " }),
		"role":     mut(func(u *User) { u.Role = "root" }),
		"status":   mut(func(u *User) { u.Status = "zombie" }),
		"units":    mut(func(u *User) { u.Units = "furlongs" }),
	} {
		if err := ValidateUserShape(u); err == nil {
			t.Errorf("%s: invalid user accepted", name)
		}
	}
	if !ok.IsActive() || mut(func(u *User) { u.Status = UserStatusInvited }).IsActive() {
		t.Error("IsActive mismatch")
	}
}

func TestUserStatusTransitions(t *testing.T) {
	const (
		inv = UserStatusInvited
		act = UserStatusActive
		sus = UserStatusSuspended
		del = UserStatusDeleted
	)
	all := []UserStatus{inv, act, sus, del}
	allowed := map[UserStatus]map[UserStatus]bool{
		inv: {inv: true, act: true, sus: true, del: true},
		act: {act: true, sus: true, del: true},
		sus: {act: true, sus: true, del: true},
		del: {del: true},
	}
	for _, from := range all {
		for _, to := range all {
			want := allowed[from][to]
			if got := from.CanTransitionTo(to); got != want {
				t.Errorf("%s -> %s = %v, want %v", from, to, got, want)
			}
			err := from.TransitionTo(to)
			if want && err != nil {
				t.Errorf("%s -> %s: unexpected %v", from, to, err)
			}
			if !want && !errors.Is(err, ErrUserStatusTransition) {
				t.Errorf("%s -> %s: err = %v, want ErrUserStatusTransition", from, to, err)
			}
		}
	}
	if err := UserStatus("bogus").TransitionTo(act); !errors.Is(err, ErrInvalidUserStatus) {
		t.Errorf("unknown from: %v", err)
	}
	if err := act.TransitionTo("bogus"); !errors.Is(err, ErrInvalidUserStatus) {
		t.Errorf("unknown to: %v", err)
	}
}
