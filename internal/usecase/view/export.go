package view

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/export"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ExportFile is a rendered interchange file.
type ExportFile struct {
	Data        []byte
	Filename    string
	ContentType string
}

// Exports renders an activity's merged stream as GPX/TCX/FIT. Shared by the
// REST download routes, the signed /content route and the Connect RPCs.
type Exports struct {
	streams port.StreamRepo
}

// NewExports wires the stream repo.
func NewExports(streams port.StreamRepo) *Exports { return &Exports{streams: streams} }

// Formats lists the accepted formats.
var Formats = []string{"gpx", "tcx", "fit"}

// ContentType of a format.
func ContentType(format string) string {
	switch format {
	case "gpx":
		return "application/gpx+xml"
	case "tcx":
		return "application/vnd.garmin.tcx+xml"
	case "fit":
		return "application/vnd.ant.fit"
	}
	return ""
}

// Stream loads the activity's primary raw stream; ActionErrorPrecondition
// when it has none.
func (e *Exports) Stream(ctx context.Context, act domain.Activity) (domain.Stream, error) {
	if act.PrimaryStreamSourceID == nil {
		return domain.Stream{}, port.NewActionError(port.ActionErrorPrecondition, "this activity has no stream to export")
	}
	stream, err := e.streams.QueryStream(ctx, domain.StreamQuery{
		ActivitySourceID: *act.PrimaryStreamSourceID,
		Resolution:       domain.StreamResolutionRaw,
	})
	if err != nil {
		return domain.Stream{}, fmt.Errorf("stream query: %w", err)
	}
	return stream, nil
}

// Available reports which export features the activity carries.
func (e *Exports) Available(ctx context.Context, act domain.Activity) (map[string]bool, error) {
	stream, err := e.Stream(ctx, act)
	if err != nil {
		return nil, err
	}
	return export.Available(act, stream), nil
}

// Render produces the file. exclude is a comma list of feature tokens.
func (e *Exports) Render(ctx context.Context, act domain.Activity, format, exclude string) (ExportFile, error) {
	ct := ContentType(format)
	if ct == "" {
		return ExportFile{}, port.NewActionError(port.ActionErrorInvalid, "unsupported format")
	}
	opts, err := export.OptionsFromExclude(exclude)
	if err != nil {
		return ExportFile{}, port.NewActionError(port.ActionErrorInvalid, err.Error())
	}
	stream, err := e.Stream(ctx, act)
	if err != nil {
		return ExportFile{}, err
	}
	if len(stream.Samples) == 0 {
		return ExportFile{}, port.NewActionError(port.ActionErrorPrecondition, "this activity has no stream samples to export")
	}
	act, stream = export.Apply(opts, act, stream)
	var data []byte
	switch format {
	case "gpx":
		data = export.GPX(act, stream)
		// GPX needs at least one GPS point; if none, steer the user to TCX.
		if !strings.Contains(string(data), "<trkpt") {
			return ExportFile{}, port.NewActionError(port.ActionErrorPrecondition, "this activity has no GPS track — export as TCX instead")
		}
	case "tcx":
		data = export.TCX(act, stream)
	case "fit":
		data, err = export.FIT(act, stream)
		if err != nil {
			return ExportFile{}, fmt.Errorf("fit encode: %w", err)
		}
	}
	return ExportFile{Data: data, Filename: export.Filename(act, format), ContentType: ct}, nil
}

// ValidFormat reports whether format is one of Formats.
func ValidFormat(format string) bool {
	for _, f := range Formats {
		if f == format {
			return true
		}
	}
	return false
}

// ErrExport is the sentinel wrapped by export failures that are not ActionErrors.
var ErrExport = errors.New("export failed")
