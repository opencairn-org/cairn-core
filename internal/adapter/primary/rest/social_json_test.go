package rest

import (
	"testing"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
)

func socialTestActivity() domain.Activity {
	lat, lng := 50.1000, 10.2000
	return domain.Activity{
		Title:      "Morning Run",
		Type:       domain.ActivityTypeRun,
		StartTime:  time.Date(2026, 6, 7, 8, 0, 0, 0, time.UTC),
		StartLat:   &lat,
		StartLng:   &lng,
		StartPlace: "Musterhausen",
	}
}

func socialLocationCats() domain.CategorySet {
	return domain.CategorySet{domain.CategorySummary: true, domain.CategoryLocation: true}
}

func TestProjectActivityJSON_NoZones_ExposesStart(t *testing.T) {
	a := socialTestActivity()
	out := projectActivityJSON(a, socialLocationCats(), nil)
	if out["start_lat"] == nil || out["start_lng"] == nil {
		t.Fatal("with no zones the start coords must be present")
	}
	if out["start_place"] != "Musterhausen" {
		t.Errorf("start_place = %v, want Musterhausen", out["start_place"])
	}
	if out["start_location_redacted"] != nil {
		t.Error("must not be redacted when no zones")
	}
}

func TestProjectActivityJSON_StartInZone_Redacted(t *testing.T) {
	a := socialTestActivity()
	zones := []domain.PrivacyZone{{Lat: 50.1000, Lng: 10.2000, RadiusM: 200}}
	out := projectActivityJSON(a, socialLocationCats(), zones)
	if out["start_lat"] != nil || out["start_lng"] != nil {
		t.Error("coords must be suppressed for a start inside a privacy zone")
	}
	if _, ok := out["start_place"]; ok {
		t.Error("start_place must be suppressed for a start inside a privacy zone")
	}
	if out["start_location_redacted"] != true {
		t.Error("start_location_redacted must be set")
	}
}

func TestProjectActivityJSON_StartOutsideZone_Exposed(t *testing.T) {
	a := socialTestActivity()
	// Zone is ~1 km away → start is outside it.
	zones := []domain.PrivacyZone{{Lat: 50.1090, Lng: 10.2000, RadiusM: 200}}
	out := projectActivityJSON(a, socialLocationCats(), zones)
	if out["start_lat"] == nil {
		t.Error("coords must be present for a start outside every zone")
	}
}

func TestProjectActivityJSON_NoLocationCategory_NoStart(t *testing.T) {
	a := socialTestActivity()
	cats := domain.CategorySet{domain.CategorySummary: true} // no location
	out := projectActivityJSON(a, cats, nil)
	if out["start_lat"] != nil || out["start_place"] != nil {
		t.Error("no location category → no start fields at all")
	}
}
