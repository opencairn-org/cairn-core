package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

// Blob key layout (docs/architecture.md §9). Workers never choose keys: core
// mints them on presign_upload and checks every key a worker hands back
// (presign_download, payload_ref, raw_blob_id, attachments) against this
// layout and the worker's provider.
//
//	users/<user_uuid>/<provider>/sources/<source>/<blob>
//	users/<user_uuid>/<provider>/uploads/<blob>
//	shared/<provider>/<blob>
//	transfer/<provider>/<blob>.json     claim-checked results, core-only
type blobKeyInfo struct {
	Provider string
	UserID   string // "" for shared/ and transfer/ keys
	Transfer bool
}

var errBlobKeyLayout = errors.New("blob key outside the known layout")

// validBlobSegment: one path segment, no traversal, S3-safe charset.
func validBlobSegment(s string) bool {
	if s == "" || len(s) > 128 || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

// parseBlobKey reads provider and owner out of a key; ok is false for any
// key that does not follow the layout.
func parseBlobKey(key string) (blobKeyInfo, bool) {
	segs := strings.Split(key, "/")
	for _, s := range segs {
		if !validBlobSegment(s) {
			return blobKeyInfo{}, false
		}
	}
	switch segs[0] {
	case "users":
		return parseUserBlobKey(segs)
	case "shared", "transfer":
		if len(segs) == 3 && domain.ValidProviderID(segs[1]) {
			return blobKeyInfo{Provider: segs[1], Transfer: segs[0] == "transfer"}, true
		}
	}
	return blobKeyInfo{}, false
}

// parseUserBlobKey handles users/<uuid>/<provider>/{sources/<src>,uploads}/<blob>.
func parseUserBlobKey(segs []string) (blobKeyInfo, bool) {
	if len(segs) < 5 || !domain.ValidProviderID(segs[2]) {
		return blobKeyInfo{}, false
	}
	if _, err := uuid.Parse(segs[1]); err != nil {
		return blobKeyInfo{}, false
	}
	sources := segs[3] == "sources" && len(segs) == 6
	uploads := segs[3] == "uploads" && len(segs) == 5
	if !sources && !uploads {
		return blobKeyInfo{}, false
	}
	return blobKeyInfo{Provider: segs[2], UserID: segs[1]}, true
}

// checkOwnedBlob accepts "" or a durable key under the provider's prefix for
// that user; a shared/ key of the provider is fine when userID is empty.
func checkOwnedBlob(key, provider string, userID string) error {
	if key == "" {
		return nil
	}
	info, ok := parseBlobKey(key)
	if !ok || info.Transfer {
		return fmt.Errorf("%w: %q", errBlobKeyLayout, key)
	}
	if info.Provider != provider {
		return fmt.Errorf("blob %q belongs to provider %q, not %q", key, info.Provider, provider)
	}
	if info.UserID != userID {
		return fmt.Errorf("blob %q belongs to another user", key)
	}
	return nil
}

// checkTransferBlob accepts a claim-check key of the provider.
func checkTransferBlob(key, provider string) error {
	info, ok := parseBlobKey(key)
	if !ok || !info.Transfer {
		return fmt.Errorf("%w: %q", errBlobKeyLayout, key)
	}
	if info.Provider != provider {
		return fmt.Errorf("payload %q belongs to provider %q, not %q", key, info.Provider, provider)
	}
	return nil
}
