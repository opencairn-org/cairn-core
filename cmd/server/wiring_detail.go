package main

import (
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/mapimage"
)

// mapImageETag derives Activity.map_image_etag for the Connect server.
func mapImageETag(act domain.Activity) string { return mapimage.ETag(act) }
