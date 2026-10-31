package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// ParserVersion is a provider parser's version "N.M" (docs/adr/0005): N is
// the dataset (archive format) it reads, M counts changes to its output.
type ParserVersion struct {
	Dataset int
	Parser  int
}

// ParseParserVersion reads "N.M"; a legacy bare "N" reads as N.0.
func ParseParserVersion(s string) (ParserVersion, error) {
	s = strings.TrimSpace(s)
	major, minor, hasMinor := strings.Cut(s, ".")
	n, err := strconv.Atoi(major)
	if err != nil || n < 0 {
		return ParserVersion{}, fmt.Errorf("parser version %q: want N.M", s)
	}
	v := ParserVersion{Dataset: n}
	if hasMinor {
		m, err := strconv.Atoi(minor)
		if err != nil || m < 0 {
			return ParserVersion{}, fmt.Errorf("parser version %q: want N.M", s)
		}
		v.Parser = m
	}
	return v, nil
}

func (v ParserVersion) String() string { return fmt.Sprintf("%d.%d", v.Dataset, v.Parser) }

// Compare orders versions: -1, 0 or +1.
func (v ParserVersion) Compare(o ParserVersion) int {
	switch {
	case v.Dataset != o.Dataset:
		return cmpInt(v.Dataset, o.Dataset)
	default:
		return cmpInt(v.Parser, o.Parser)
	}
}

// CanReparse reports whether a parser at v may re-parse an archive whose
// source was last parsed by stored: same dataset, never backwards.
func (v ParserVersion) CanReparse(stored ParserVersion) bool {
	return v.Dataset == stored.Dataset && v.Parser >= stored.Parser
}

// ReparseUpgrade classifies what a live parser v offers a source parsed by
// stored.
type ReparseUpgrade int

const (
	UpgradeNone       ReparseUpgrade = iota // same version, or v is older
	UpgradeNewParser                        // same dataset, newer output: re-parse from the archive
	UpgradeNewDataset                       // newer archive format: re-fetch only
)

// Reimport reasons stored on a flagged source (reimport_status_reason).
const (
	ReimportReasonNewerParser  = "newer_parser"
	ReimportReasonNewerDataset = "newer_dataset"
)

// Reason is the reimport reason a source flagged for u carries; "" for none.
func (u ReparseUpgrade) Reason() string {
	switch u {
	case UpgradeNewParser:
		return ReimportReasonNewerParser
	case UpgradeNewDataset:
		return ReimportReasonNewerDataset
	}
	return ""
}

func (v ParserVersion) UpgradeFor(stored ParserVersion) ReparseUpgrade {
	switch {
	case v.Dataset > stored.Dataset:
		return UpgradeNewDataset
	case v.Dataset == stored.Dataset && v.Parser > stored.Parser:
		return UpgradeNewParser
	default:
		return UpgradeNone
	}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// CheckParserForward rejects re-ingesting src with an older parser of the
// same package. Unparseable (legacy, internal) versions are not compared.
func CheckParserForward(src ActivitySource, pkg, version string) error {
	if pkg == "" || pkg != src.SourceWorkerPackage {
		return nil
	}
	in, err := ParseParserVersion(version)
	if err != nil {
		return nil
	}
	stored, err := ParseParserVersion(src.SourceWorkerVersion)
	if err != nil {
		return nil
	}
	if in.Compare(stored) < 0 {
		return fmt.Errorf("%w: %s < %s", ErrParserDowngrade, in, stored)
	}
	return nil
}
