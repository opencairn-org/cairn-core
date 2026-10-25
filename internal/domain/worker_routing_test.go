package domain

import (
	"errors"
	"testing"
)

func TestResolveAccountWorker(t *testing.T) {
	for _, c := range []struct {
		assigned string
		live     []string
		want     string
		persist  bool
		err      error
	}{
		{"a", nil, "a", false, nil},
		{"a", []string{"b"}, "a", false, nil},
		{"", []string{"b", "b"}, "b", true, nil},
		{"", nil, "", false, ErrNoWorker},
		{"", []string{"b", "c"}, "", false, ErrWorkerAmbiguous},
	} {
		got, persist, err := ResolveAccountWorker(c.assigned, c.live)
		if got != c.want || persist != c.persist || !errors.Is(err, c.err) {
			t.Errorf("%q %v = %q %v %v", c.assigned, c.live, got, persist, err)
		}
	}
}

func TestResolveConnectPool(t *testing.T) {
	for _, c := range []struct {
		requested, existing string
		live                []string
		want                string
		err                 error
	}{
		{"b", "", []string{"a", "b"}, "b", nil},
		{"x", "", []string{"a", "b"}, "", ErrNoWorker},
		{"", "a", []string{"a", "b"}, "a", nil},
		{"", "", nil, "", nil},
		{"", "", []string{"a", "a"}, "a", nil},
		{"", "", []string{"a", "b"}, "", ErrWorkerAmbiguous},
	} {
		got, err := ResolveConnectPool(c.requested, c.existing, c.live)
		if got != c.want || !errors.Is(err, c.err) {
			t.Errorf("%+v = %q %v", c, got, err)
		}
	}
}
