package main

// deref is still used by admin_users.go and segments_author.go.
func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
