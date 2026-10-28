package domain

import "strconv"

// LiveWorker is the identity of a currently-online worker that matters for
// re-parse: provider + parser version + package. Those three ARE the
// compatibility contract; the name is an admin-assigned routing label.
type LiveWorker struct {
	Name       string // pool name: jobs go to cairn.jobs.<job>.<provider>.<name>
	Provider   string
	Version    string // parser version N.M
	Package    string
	Release    string
	Compatible bool // release matches core's major.minor (or either is untagged)
}

// ReparseWorker returns the newest live, release-compatible worker allowed to
// re-parse src's archive (docs/adr/0005): same provider and package, same
// dataset, parser not older than the one that last parsed it. A source with a
// non-N.M version only matches its exact version.
func ReparseWorker(workers []LiveWorker, src ActivitySource) (LiveWorker, bool) {
	if src.SourceWorkerPackage == "" {
		return LiveWorker{}, false // legacy source without package provenance
	}
	stored, serr := ParseParserVersion(src.SourceWorkerVersion)
	var best LiveWorker
	var bestV ParserVersion
	found := false
	for _, w := range workers {
		if !w.Compatible || w.Provider != src.Provider || w.Package != src.SourceWorkerPackage {
			continue
		}
		wv, werr := ParseParserVersion(w.Version)
		switch {
		case serr != nil || werr != nil:
			if w.Version != src.SourceWorkerVersion {
				continue
			}
		case !wv.CanReparse(stored):
			continue
		}
		if !found || wv.Compare(bestV) > 0 {
			best, bestV, found = w, wv, true
		}
	}
	return best, found
}

// SourceUpgrade says what the live workers offer src beyond its current
// parse: "new_parser" (re-parse from the archive), "new_dataset" (re-fetch)
// or "".
func SourceUpgrade(workers []LiveWorker, src ActivitySource) string {
	stored, err := ParseParserVersion(src.SourceWorkerVersion)
	if err != nil {
		return ""
	}
	up := UpgradeNone
	for _, w := range workers {
		if !w.Compatible || w.Provider != src.Provider || w.Package != src.SourceWorkerPackage {
			continue
		}
		if wv, err := ParseParserVersion(w.Version); err == nil {
			up = max(up, wv.UpgradeFor(stored))
		}
	}
	switch up {
	case UpgradeNewParser:
		return "new_parser"
	case UpgradeNewDataset:
		return "new_dataset"
	}
	return ""
}

// SourceInsight is one source row of the activity manage view.
type SourceInsight struct {
	Source          ActivitySource
	ParserVersion   string // N.M when parseable, else the raw version
	DatasetVersion  string // "" when not parseable
	ReparseWith     string // parser version of the worker that would re-parse, "" when none
	Upgrade         string // "" | new_parser | new_dataset
	ReparseEligible bool
	IsPrimary       bool
	WonFieldGroups  []string // labels of the field groups this source won
}

// FieldProvenance says which source won a field group and why.
type FieldProvenance struct {
	Field      FieldGroup
	FieldLabel string
	SourceID   SourceID
	Provider   string
	DecidedBy  string // "rule" | "manual"
}

// ManageInfo is the technical view of an activity's sources and merge.
type ManageInfo struct {
	Activity   Activity
	Primary    *SourceID
	Sources    []SourceInsight
	Provenance []FieldProvenance // sorted by field
}

// fieldGroupLabels maps the merge engine's field-group keys to user-facing
// labels; unknown keys fall back to the raw key.
var fieldGroupLabels = map[FieldGroup]string{
	FieldGroupClassification: "Type & flags",
	FieldGroupTime:           "Time",
	FieldGroupDuration:       "Duration",
	FieldGroupTitle:          "Title",
	FieldGroupDescription:    "Description",
	FieldGroupDistance:       "Distance",
	FieldGroupElevation:      "Elevation",
	FieldGroupSpeed:          "Speed",
	FieldGroupHeartRate:      "Heart rate",
	FieldGroupPower:          "Power",
	FieldGroupCadence:        "Cadence",
	FieldGroupTemperature:    "Temperature",
	FieldGroupCalories:       "Calories",
	FieldGroupTrainingLoad:   "Training load",
	FieldGroupSwim:           "Swim",
	FieldGroupGPSTrack:       "GPS track",
	FieldGroupLaps:           "Laps",
}

// FieldGroupLabel is the user-facing label of a field group.
func FieldGroupLabel(g FieldGroup) string {
	if l, ok := fieldGroupLabels[g]; ok {
		return l
	}
	return string(g)
}

// BuildManageInfo assembles the manage view. manualFields are the field
// groups pinned by the user (field_source_overrides); workers are the live
// workers deciding re-parse eligibility and offered upgrades.
func BuildManageInfo(act Activity, sources []ActivitySource, workers []LiveWorker, manualFields map[FieldGroup]bool) ManageInfo {
	info := ManageInfo{Activity: act, Primary: act.PrimaryStreamSourceID}
	wonBy := map[SourceID][]string{}
	providerBySource := map[SourceID]string{}
	for _, s := range sources {
		providerBySource[s.ID] = s.Provider
	}
	for g, sid := range act.MergeProvenance {
		wonBy[sid] = append(wonBy[sid], FieldGroupLabel(g))
		decidedBy := "rule"
		if manualFields[g] {
			decidedBy = "manual"
		}
		info.Provenance = append(info.Provenance, FieldProvenance{Field: g, FieldLabel: FieldGroupLabel(g), SourceID: sid, Provider: providerBySource[sid], DecidedBy: decidedBy})
	}
	sortProvenance(info.Provenance)
	for _, s := range sources {
		info.Sources = append(info.Sources, sourceInsight(s, workers, act.PrimaryStreamSourceID, wonBy[s.ID]))
	}
	return info
}

func sortProvenance(p []FieldProvenance) {
	for i := 1; i < len(p); i++ {
		for j := i; j > 0 && p[j].Field < p[j-1].Field; j-- {
			p[j], p[j-1] = p[j-1], p[j]
		}
	}
}

func sourceInsight(s ActivitySource, workers []LiveWorker, primary *SourceID, won []string) SourceInsight {
	in := SourceInsight{Source: s, WonFieldGroups: won, IsPrimary: primary != nil && *primary == s.ID, Upgrade: SourceUpgrade(workers, s)}
	in.ParserVersion = s.SourceWorkerVersion
	if v, err := ParseParserVersion(s.SourceWorkerVersion); err == nil {
		in.ParserVersion, in.DatasetVersion = v.String(), strconv.Itoa(v.Dataset)
	}
	if rw, ok := ReparseWorker(workers, s); ok && s.RawBlobID != "" {
		in.ReparseEligible = true
		in.ReparseWith = rw.Version
	}
	return in
}
