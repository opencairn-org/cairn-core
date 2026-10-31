package main

import (
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/mapimage"
)

// mapImageETag is still referenced by connect_rpc.go.
func mapImageETag(act domain.Activity) string { return mapimage.ETag(act) }
