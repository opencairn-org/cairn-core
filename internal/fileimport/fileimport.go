// Package fileimport decodes user-supplied activity input (GPX/TCX/FIT files,
// manual entries) into the typed payload the worker ingest path takes.
package fileimport

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/tormoder/fit"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	cairnv1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
)

// Ext returns the lower-cased extension (with dot) of a filename.
func Ext(filename string) string {
	if i := strings.LastIndex(filename, "."); i >= 0 {
		return strings.ToLower(filename[i:])
	}
	return ""
}

// ContentType maps a file extension to a content type for the archived raw
// blob (so a later download serves it with a sensible type).
func ContentType(ext string) string {
	switch ext {
	case ".gpx":
		return "application/gpx+xml"
	case ".tcx":
		return "application/vnd.garmin.tcx+xml"
	case ".fit":
		return "application/vnd.ant.fit"
	default:
		return "application/octet-stream"
	}
}

// Parse decodes a GPX, TCX or FIT file, dispatching on extension / content sniffing.
func Parse(filename string, raw []byte) (*cairnv1.ActivitySourcePayload, *cairnv1.ActivityStream, error) {
	name := strings.ToLower(filename)
	head := strings.ToLower(string(raw[:min(512, len(raw))]))
	switch {
	case strings.HasSuffix(name, ".gpx") || strings.Contains(head, "<gpx"):
		return parseGPX(raw)
	case strings.HasSuffix(name, ".tcx") || strings.Contains(head, "trainingcenterdatabase"):
		return parseTCX(raw)
	case strings.HasSuffix(name, ".fit") || (len(raw) > 12 && string(raw[8:12]) == ".FIT"):
		return parseFIT(raw)
	}
	return nil, nil, fmt.Errorf("unrecognised file type %q (supported: .gpx, .tcx, .fit)", filename)
}

// ---------------------------------------------------------------------------
// GPX
// ---------------------------------------------------------------------------

type gpxFile struct {
	XMLName xml.Name `xml:"gpx"`
	Trk     []struct {
		Name string `xml:"name"`
		Type string `xml:"type"`
		Seg  []struct {
			Pt []gpxPt `xml:"trkpt"`
		} `xml:"trkseg"`
	} `xml:"trk"`
}

type gpxPt struct {
	Lat  float64  `xml:"lat,attr"`
	Lon  float64  `xml:"lon,attr"`
	Ele  *float64 `xml:"ele"`
	Time string   `xml:"time"`
	Ext  struct {
		// Garmin TrackPointExtension (matched by local name, ignoring ns).
		HR    *int     `xml:"TrackPointExtension>hr"`
		Cad   *int     `xml:"TrackPointExtension>cad"`
		ATemp *float64 `xml:"TrackPointExtension>atemp"`
		// Power conventions vary across exporters.
		Power        *int `xml:"power"`
		PowerInWatts *int `xml:"PowerInWatts"`
	} `xml:"extensions"`
}

func parseGPX(raw []byte) (*cairnv1.ActivitySourcePayload, *cairnv1.ActivityStream, error) {
	var g gpxFile
	if err := xml.Unmarshal(raw, &g); err != nil { //nolint:gosec // G709: encoding/xml resolves no external entities; decodes into a fixed struct
		return nil, nil, fmt.Errorf("gpx: %w", err)
	}
	if len(g.Trk) == 0 {
		return nil, nil, fmt.Errorf("gpx: no tracks")
	}

	var pts []gpxPt
	for _, trk := range g.Trk {
		for _, seg := range trk.Seg {
			pts = append(pts, seg.Pt...)
		}
	}
	if len(pts) == 0 {
		return nil, nil, fmt.Errorf("gpx: no track points")
	}

	b := newActivityBuilder(g.Trk[0].Name, g.Trk[0].Type)
	for _, p := range pts {
		t, _ := time.Parse(time.RFC3339, p.Time)
		var power *int
		if p.Ext.Power != nil {
			power = p.Ext.Power
		} else if p.Ext.PowerInWatts != nil {
			power = p.Ext.PowerInWatts
		}
		b.add(samplePoint{
			Time: t, HasLatLon: true, Lat: p.Lat, Lon: p.Lon,
			Alt: p.Ele, HR: p.Ext.HR, Cad: p.Ext.Cad, Power: power, Temp: p.Ext.ATemp,
		})
	}
	return b.finish()
}

// ---------------------------------------------------------------------------
// TCX
// ---------------------------------------------------------------------------

type tcxFile struct {
	XMLName    xml.Name `xml:"TrainingCenterDatabase"`
	Activities struct {
		Activity []struct {
			Sport string `xml:"Sport,attr"`
			Lap   []struct {
				Track struct {
					Pt []tcxPt `xml:"Trackpoint"`
				} `xml:"Track"`
			} `xml:"Lap"`
		} `xml:"Activity"`
	} `xml:"Activities"`
}

type tcxPt struct {
	Time     string `xml:"Time"`
	Position *struct {
		Lat float64 `xml:"LatitudeDegrees"`
		Lon float64 `xml:"LongitudeDegrees"`
	} `xml:"Position"`
	Alt   *float64 `xml:"AltitudeMeters"`
	Dist  *float64 `xml:"DistanceMeters"`
	Cad   *int     `xml:"Cadence"`
	HRBpm *struct {
		Value int `xml:"Value"`
	} `xml:"HeartRateBpm"`
	Ext struct {
		Watts *int     `xml:"TPX>Watts"`
		Speed *float64 `xml:"TPX>Speed"`
	} `xml:"Extensions"`
}

func parseTCX(raw []byte) (*cairnv1.ActivitySourcePayload, *cairnv1.ActivityStream, error) {
	var x tcxFile
	if err := xml.Unmarshal(raw, &x); err != nil { //nolint:gosec // G709: encoding/xml resolves no external entities; decodes into a fixed struct
		return nil, nil, fmt.Errorf("tcx: %w", err)
	}
	if len(x.Activities.Activity) == 0 {
		return nil, nil, fmt.Errorf("tcx: no activities")
	}
	act := x.Activities.Activity[0]

	b := newActivityBuilder("", act.Sport)
	for _, lap := range act.Lap {
		for _, p := range lap.Track.Pt {
			t, _ := time.Parse(time.RFC3339, p.Time)
			sp := samplePoint{Time: t, Alt: p.Alt, Dist: p.Dist, Cad: p.Cad, Power: p.Ext.Watts, Speed: p.Ext.Speed}
			if p.Position != nil {
				sp.HasLatLon, sp.Lat, sp.Lon = true, p.Position.Lat, p.Position.Lon
			}
			if p.HRBpm != nil {
				hr := p.HRBpm.Value
				sp.HR = &hr
			}
			b.add(sp)
		}
	}
	return b.finish()
}

// ---------------------------------------------------------------------------
// FIT (Garmin/Wahoo/etc. binary)
// ---------------------------------------------------------------------------

func parseFIT(raw []byte) (*cairnv1.ActivitySourcePayload, *cairnv1.ActivityStream, error) {
	f, err := fit.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, fmt.Errorf("fit: %w", err)
	}
	act, err := f.Activity()
	if err != nil {
		return nil, nil, fmt.Errorf("fit: not an activity file: %w", err)
	}

	sport := ""
	if len(act.Sessions) > 0 {
		sport = act.Sessions[0].Sport.String()
	}

	b := newActivityBuilder("", sport)
	for _, rec := range act.Records {
		if rec.Timestamp.IsZero() {
			continue
		}
		b.add(fitRecordSample(rec))
	}
	return b.finish()
}

// fitRecordSample converts one FIT record, skipping invalid (sentinel) fields.
func fitRecordSample(rec *fit.RecordMsg) samplePoint {
	sp := samplePoint{Time: rec.Timestamp}
	if !rec.PositionLat.Invalid() && !rec.PositionLong.Invalid() {
		sp.HasLatLon = true
		sp.Lat = rec.PositionLat.Degrees()
		sp.Lon = rec.PositionLong.Degrees()
	}
	if v := rec.GetAltitudeScaled(); !math.IsNaN(v) {
		sp.Alt = &v
	}
	if v := rec.GetDistanceScaled(); !math.IsNaN(v) {
		sp.Dist = &v
	}
	if v := rec.GetSpeedScaled(); !math.IsNaN(v) {
		sp.Speed = &v
	}
	if rec.HeartRate != 0xFF {
		hr := int(rec.HeartRate)
		sp.HR = &hr
	}
	if rec.Cadence != 0xFF {
		c := int(rec.Cadence)
		sp.Cad = &c
	}
	if rec.Power != 0xFFFF {
		p := int(rec.Power)
		sp.Power = &p
	}
	if rec.Temperature != 0x7F {
		t := float64(rec.Temperature)
		sp.Temp = &t
	}
	return sp
}

// ---------------------------------------------------------------------------
// Shared builder: points → typed payload + column stream
// ---------------------------------------------------------------------------

type samplePoint struct {
	Time      time.Time
	HasLatLon bool
	Lat, Lon  float64
	Alt       *float64
	Dist      *float64
	Speed     *float64
	HR        *int
	Cad       *int
	Power     *int
	Temp      *float64
}

type activityBuilder struct {
	name, sport string
	pts         []samplePoint
}

func newActivityBuilder(name, sport string) *activityBuilder {
	return &activityBuilder{name: name, sport: sport}
}

func (b *activityBuilder) add(p samplePoint) {
	if p.Time.IsZero() {
		return
	}
	b.pts = append(b.pts, p)
}

// columns accumulates the per-sample stream columns and running totals.
type columns struct {
	lat, lon, alt, dist, speed, temp              []float64
	hr, power, cad                                []int32
	haveLL, haveAlt, haveDist                     bool
	haveSpeed, haveHR, havePow, haveCad, haveTemp bool
	cumDist                                       float64
	sumHR, cntHR, sumPow, cntPow                  float64
	elevGain                                      float64
	prevAlt                                       *float64
}

func newColumns(n int) *columns {
	return &columns{
		lat: mkF(n), lon: mkF(n), alt: mkF(n), dist: mkF(n), speed: mkF(n),
		hr: mkI(n), power: mkI(n), cad: mkI(n),
		temp: mkF(n),
	}
}

// clampInt32 converts an int sample value, saturating at the int32 range.
func clampInt32(v int) int32 {
	return int32(max(math.MinInt32, min(v, math.MaxInt32))) //nolint:gosec // G115: clamped to int32 range
}

// addGeo records position, distance and altitude for sample i.
func (c *columns) addGeo(pts []samplePoint, i int) {
	p := pts[i]
	if p.HasLatLon {
		c.lat[i], c.lon[i] = p.Lat, p.Lon
		c.haveLL = true
		if i > 0 && pts[i-1].HasLatLon {
			c.cumDist += haversine(pts[i-1].Lat, pts[i-1].Lon, p.Lat, p.Lon)
		}
	}
	if p.Dist != nil {
		c.dist[i] = *p.Dist
		c.haveDist = true
	} else if c.haveLL {
		c.dist[i] = c.cumDist
		c.haveDist = true
	}
	if p.Alt != nil {
		c.alt[i] = *p.Alt
		c.haveAlt = true
		if c.prevAlt != nil && *p.Alt > *c.prevAlt {
			c.elevGain += *p.Alt - *c.prevAlt
		}
		c.prevAlt = p.Alt
	}
}

// addSensors records speed, HR, power, cadence and temperature for sample i.
func (c *columns) addSensors(p samplePoint, i int) {
	if p.Speed != nil {
		c.speed[i] = *p.Speed
		c.haveSpeed = true
	}
	if p.HR != nil {
		c.hr[i] = clampInt32(*p.HR)
		c.haveHR = true
		c.sumHR += float64(*p.HR)
		c.cntHR++
	}
	if p.Power != nil {
		c.power[i] = clampInt32(*p.Power)
		c.havePow = true
		c.sumPow += float64(*p.Power)
		c.cntPow++
	}
	if p.Cad != nil {
		c.cad[i] = clampInt32(*p.Cad)
		c.haveCad = true
	}
	if p.Temp != nil {
		c.temp[i] = *p.Temp
		c.haveTemp = true
	}
}

// fillStream attaches the present columns to st and lists their channels.
func (c *columns) fillStream(st *cairnv1.ActivityStream) {
	var ch []cairnv1.StreamChannel
	if c.haveLL {
		st.Latitude, st.Longitude = c.lat, c.lon
		ch = append(ch, cairnv1.StreamChannel_STREAM_CHANNEL_LATITUDE, cairnv1.StreamChannel_STREAM_CHANNEL_LONGITUDE)
	}
	if c.haveAlt {
		st.AltitudeM = c.alt
		ch = append(ch, cairnv1.StreamChannel_STREAM_CHANNEL_ALTITUDE)
	}
	if c.haveDist {
		st.DistanceM = c.dist
		ch = append(ch, cairnv1.StreamChannel_STREAM_CHANNEL_DISTANCE)
	}
	if c.haveSpeed {
		st.SpeedMps = c.speed
		ch = append(ch, cairnv1.StreamChannel_STREAM_CHANNEL_SPEED)
	}
	if c.haveHR {
		st.HeartRateBpm = c.hr
		ch = append(ch, cairnv1.StreamChannel_STREAM_CHANNEL_HEART_RATE)
	}
	if c.havePow {
		st.PowerW = c.power
		ch = append(ch, cairnv1.StreamChannel_STREAM_CHANNEL_POWER)
	}
	if c.haveCad {
		st.Cadence = c.cad
		ch = append(ch, cairnv1.StreamChannel_STREAM_CHANNEL_CADENCE)
	}
	if c.haveTemp {
		st.TemperatureC = c.temp
		ch = append(ch, cairnv1.StreamChannel_STREAM_CHANNEL_TEMPERATURE)
	}
	st.Channels = ch
}

// summary derives the activity summary from the accumulated columns.
func (c *columns) summary(n int) *cairnv1.ActivitySummary {
	summary := &cairnv1.ActivitySummary{}
	if c.haveDist {
		d := c.dist[n-1]
		summary.DistanceM = &d
	}
	if c.haveAlt {
		summary.ElevationGainM = &c.elevGain
	}
	if c.cntHR > 0 {
		v := int32(c.sumHR / c.cntHR)
		summary.AvgHeartRateBpm = &v
	}
	if c.cntPow > 0 {
		v := int32(c.sumPow / c.cntPow)
		summary.AvgPowerW = &v
	}
	return summary
}

func (b *activityBuilder) finish() (*cairnv1.ActivitySourcePayload, *cairnv1.ActivityStream, error) {
	if len(b.pts) == 0 {
		return nil, nil, fmt.Errorf("no timestamped track points")
	}
	start := b.pts[0].Time
	end := b.pts[len(b.pts)-1].Time

	typ, disc := mapSport(b.sport)
	title := b.name
	if title == "" {
		title = "Imported Activity"
	}

	n := len(b.pts)
	st := &cairnv1.ActivityStream{SampleCount: clampInt32(n), ResolutionHz: 1.0}
	st.TimeS = make([]float64, n)

	c := newColumns(n)
	for i, p := range b.pts {
		st.TimeS[i] = p.Time.Sub(start).Seconds()
		c.addGeo(b.pts, i)
		c.addSensors(p, i)
	}
	c.fillStream(st)

	payload := &cairnv1.ActivitySourcePayload{
		Type:               typ,
		Discipline:         disc,
		ProviderNativeType: b.sport,
		Title:              title,
		StartTime:          timestamppb.New(start.UTC()),
		EndTime:            timestamppb.New(end.UTC()),
		ElapsedDuration:    durationpb.New(end.Sub(start)),
		MovingDuration:     durationpb.New(end.Sub(start)),
		Timezone:           "UTC",
		Summary:            c.summary(n),
	}
	return payload, st, nil
}

func mapSport(sport string) (cairnv1.ActivityType, cairnv1.Discipline) {
	s := strings.ToLower(sport)
	switch {
	case strings.Contains(s, "cycl"), strings.Contains(s, "ride"), strings.Contains(s, "bike"), strings.Contains(s, "biking"):
		return cairnv1.ActivityType_ACTIVITY_TYPE_RIDE, cairnv1.Discipline_DISCIPLINE_RIDE_ROAD
	case strings.Contains(s, "run"):
		return cairnv1.ActivityType_ACTIVITY_TYPE_RUN, cairnv1.Discipline_DISCIPLINE_RUN_ROAD
	case strings.Contains(s, "hik"):
		return cairnv1.ActivityType_ACTIVITY_TYPE_HIKE, cairnv1.Discipline_DISCIPLINE_UNSPECIFIED
	case strings.Contains(s, "walk"):
		return cairnv1.ActivityType_ACTIVITY_TYPE_WALK, cairnv1.Discipline_DISCIPLINE_UNSPECIFIED
	case strings.Contains(s, "swim"):
		return cairnv1.ActivityType_ACTIVITY_TYPE_SWIM, cairnv1.Discipline_DISCIPLINE_UNSPECIFIED
	}
	return cairnv1.ActivityType_ACTIVITY_TYPE_WORKOUT, cairnv1.Discipline_DISCIPLINE_UNSPECIFIED
}

func mkF(n int) []float64 { return make([]float64, n) }
func mkI(n int) []int32   { return make([]int32, n) }

// haversine returns the great-circle distance in metres.
func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371000.0
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return R * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}
