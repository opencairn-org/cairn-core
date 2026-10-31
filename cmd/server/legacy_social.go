package main

import (
	"github.com/opencairn-org/cairn-core/internal/usecase/social"
)

// Leftovers of the social group still used by handlers in this package.

// truncateUTF8 caps s at maxBytes without splitting a rune.
func truncateUTF8(s string, maxBytes int) string { return social.TruncateUTF8(s, maxBytes) }

// resolveModeratorUser (admin_api.go) lost its last caller with the moderation move; drop both together.
var _ = resolveModeratorUser
