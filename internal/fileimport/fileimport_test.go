package fileimport

import (
	"strings"
	"testing"

	cairnv1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/port"
)

const gpx = `<?xml version="1.0"?>
<gpx xmlns:gpxtpx="http://www.garmin.com/xmlschemas/TrackPointExtension/v1">
<trk><name>Morning Ride</name><type>cycling</type><trkseg>
<trkpt lat="47.0" lon="8.0"><ele>400</ele><time>2026-01-01T08:00:00Z</time><extensions><gpxtpx:TrackPointExtension><gpxtpx:hr>120</gpxtpx:hr></gpxtpx:TrackPointExtension><power>200</power></extensions></trkpt>
<trkpt lat="47.001" lon="8.0"><ele>410</ele><time>2026-01-01T08:00:10Z</time><extensions><gpxtpx:TrackPointExtension><gpxtpx:hr>140</gpxtpx:hr></gpxtpx:TrackPointExtension><power>300</power></extensions></trkpt>
</trkseg></trk></gpx>`

const tcx = `<?xml version="1.0"?>
<TrainingCenterDatabase><Activities><Activity Sport="Running"><Lap><Track>
<Trackpoint><Time>2026-01-01T08:00:00Z</Time><DistanceMeters>0</DistanceMeters><HeartRateBpm><Value>150</Value></HeartRateBpm></Trackpoint>
<Trackpoint><Time>2026-01-01T08:01:00Z</Time><DistanceMeters>250</DistanceMeters><HeartRateBpm><Value>160</Value></HeartRateBpm></Trackpoint>
</Track></Lap></Activity></Activities></TrainingCenterDatabase>`

func TestParseGPX(t *testing.T) {
	p, st, err := Parse("ride.gpx", []byte(gpx))
	if err != nil {
		t.Fatal(err)
	}
	if p.GetType() != cairnv1.ActivityType_ACTIVITY_TYPE_RIDE || p.GetTitle() != "Morning Ride" {
		t.Errorf("type/title: %v %q", p.GetType(), p.GetTitle())
	}
	if p.GetElapsedDuration().AsDuration().Seconds() != 10 {
		t.Errorf("elapsed %v", p.GetElapsedDuration().AsDuration())
	}
	s := p.GetSummary()
	if d := s.GetDistanceM(); d < 100 || d > 120 {
		t.Errorf("distance %v, want ~111 m", d)
	}
	if s.GetElevationGainM() != 10 || s.GetAvgHeartRateBpm() != 130 || s.GetAvgPowerW() != 250 {
		t.Errorf("summary %v", s)
	}
	if st.GetSampleCount() != 2 || len(st.GetLatitude()) != 2 || len(st.GetPowerW()) != 2 {
		t.Errorf("stream %v", st)
	}
}

func TestParseTCXBySniffing(t *testing.T) {
	p, st, err := Parse("export.xml", []byte(tcx))
	if err != nil {
		t.Fatal(err)
	}
	if p.GetType() != cairnv1.ActivityType_ACTIVITY_TYPE_RUN || p.GetTitle() != "Imported Activity" {
		t.Errorf("type/title: %v %q", p.GetType(), p.GetTitle())
	}
	if p.GetSummary().GetDistanceM() != 250 || len(st.GetLatitude()) != 0 {
		t.Errorf("distance %v, lat %d", p.GetSummary().GetDistanceM(), len(st.GetLatitude()))
	}
}

func TestParseRejects(t *testing.T) {
	for name, raw := range map[string]string{
		"notes.txt": "hello",
		"empty.gpx": `<gpx></gpx>`,
		"bad.fit":   "0123456789ab",
	} {
		if _, _, err := Parse(name, []byte(raw)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestExtAndContentType(t *testing.T) {
	if Ext("Ride.GPX") != ".gpx" || Ext("noext") != "" {
		t.Error("ext")
	}
	if ContentType(".fit") != "application/vnd.ant.fit" || ContentType(".zip") != "application/octet-stream" {
		t.Error("content type")
	}
}

func TestManual(t *testing.T) {
	ok := port.ManualActivity{Type: "ride", Discipline: "ride_road", StartTime: "2026-01-01T08:00:00Z", ElapsedSeconds: 3600, MovingSeconds: 7200}
	p, err := Manual(ok)
	if err != nil {
		t.Fatal(err)
	}
	if p.GetTitle() != "Manual Activity" || p.GetTimezone() != "UTC" || p.GetMovingDuration().AsDuration().Hours() != 1 {
		t.Errorf("defaults: %q %q %v", p.GetTitle(), p.GetTimezone(), p.GetMovingDuration().AsDuration())
	}
	for want, in := range map[string]port.ManualActivity{
		"invalid or missing type": {StartTime: ok.StartTime, ElapsedSeconds: 1},
		"invalid start_time":      {Type: "ride", StartTime: "yesterday", ElapsedSeconds: 1},
		"elapsed_duration_s":      {Type: "ride", StartTime: ok.StartTime},
		"invalid discipline":      {Type: "ride", Discipline: "nope", StartTime: ok.StartTime, ElapsedSeconds: 1},
	} {
		if _, err := Manual(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
}
