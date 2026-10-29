//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/config"
	"github.com/opencairn-org/cairn-core/internal/db"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/httpsig"
)

// The API suite drives the real mux (all REST + Connect-RPC mounts, real Postgres, no NATS/S3)
// over HTTP. It runs in its own throwaway database so it never races the postgres package's
// migrations on the shared CAIRN_TEST_DATABASE_URL.

// itestDB creates a fresh database next to CAIRN_TEST_DATABASE_URL, migrates it and drops it on cleanup.
func itestDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	base := os.Getenv("CAIRN_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("CAIRN_TEST_DATABASE_URL unset — skipping integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	name := "cairn_itest_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	_ = admin.Close(ctx)
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	dsn := u.String()

	pool, err := db.Open(ctx, db.Config{URL: dsn, MaxConns: 8, MinConns: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.Connect(context.Background(), base)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	if _, err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool, dsn
}

// tileServer serves a blank PNG for every map tile so map rendering stays offline.
func tileServer(t *testing.T) *httptest.Server {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 256, 256))
	for i := range img.Pix {
		img.Pix[i] = 0xEE
	}
	img.Set(0, 0, color.Black)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(buf.Bytes())
	}))
	t.Cleanup(srv.Close)
	return srv
}

type itestEnv struct {
	t   *testing.T
	srv *httptest.Server
	app *App
}

type itestClient struct {
	env    *itestEnv
	http   *http.Client
	userID domain.UserID
	name   string
	bearer string
}

func newITestEnv(t *testing.T) *itestEnv {
	t.Helper()
	pool, dsn := itestDB(t)
	tiles := tileServer(t)
	for k, v := range map[string]string{
		"CAIRN_DATABASE_URL":                      dsn,
		"CAIRN_AUTH_SESSION_SECRET":               "itest-session-secret",
		"CAIRN_AUTH_MASTER_ENCRYPTION_KEY":        "itest-master-encryption-key",
		"CAIRN_AUTH_ARGON2_MEMORY":                "1024",
		"CAIRN_AUTH_ARGON2_TIME":                  "1",
		"CAIRN_GEOCODER_ENABLED":                  "false",
		"CAIRN_MAP_TILE_URL":                      tiles.URL + "/{z}/{x}/{y}.png",
		"CAIRN_FEDERATION_ENABLED":                "true",
		"CAIRN_FEDERATION_ALLOW_PRIVATE_HOSTS":    "true",
		"CAIRN_EMAIL_ADAPTER":                     "none",
		"CAIRN_NATS_URL":                          "",
		"CAIRN_STORAGE_ACCESS_KEY_ID":             "",
		"CAIRN_INSTANCE_BOOTSTRAP_ADMIN_EMAIL":    "",
		"CAIRN_INSTANCE_BOOTSTRAP_ADMIN_PASSWORD": "",
	} {
		t.Setenv(k, v)
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(securityHeaders(mux))
	t.Cleanup(srv.Close)
	// localhost, not 127.0.0.1: WebAuthn refuses an IP address as relying-party ID.
	t.Setenv("CAIRN_HTTP_PUBLIC_BASE_URL", strings.Replace(srv.URL, "127.0.0.1", "localhost", 1))

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	app, err := newApp(ctx, pool, cfg, testLogger())
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	if err := serveBootstrapAdmin(ctx, app, cfg, testLogger()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	serveMountCore(ctx, mux, app, pool, cfg, testLogger())
	serveMountFeatures(mux, app, cfg, testLogger())
	serveMountPlatform(mux, app, testLogger())
	return &itestEnv{t: t, srv: srv, app: app}
}

// user creates an account directly through the repo and signs it in over /auth/password.
func (e *itestEnv) user(name string, role domain.UserRole) *itestClient {
	e.t.Helper()
	ctx := context.Background()
	hash, err := e.app.PasswordHasher.Hash("correct horse battery")
	if err != nil {
		e.t.Fatal(err)
	}
	id := domain.UserID(uuid.New())
	if err := e.app.Users.CreateUser(ctx, domain.User{
		ID: id, Username: name, Email: name + "@example.com", DisplayName: strings.ToUpper(name[:1]) + name[1:],
		Role: role, Status: domain.UserStatusActive, Units: domain.UserUnitsMetric, EmailVerified: true,
	}, domain.Credentials{UserID: id, PasswordHash: hash}); err != nil {
		e.t.Fatalf("create user %s: %v", name, err)
	}
	c := e.anon()
	c.userID, c.name = id, name
	resp, err := c.http.PostForm(e.srv.URL+"/auth/password", url.Values{
		"identifier": {name}, "password": {"correct horse battery"}, "redirect": {"/activities"},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/activities" {
		e.t.Fatalf("login %s: %d %s", name, resp.StatusCode, resp.Header.Get("Location"))
	}
	return c
}

func (e *itestEnv) anon() *itestClient {
	jar, _ := cookiejar.New(nil)
	return &itestClient{env: e, http: &http.Client{
		Jar:           jar,
		Timeout:       60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// do sends a request and fails unless the status is one of want (default: any non-5xx).
func (c *itestClient) do(method, path string, body any, want ...int) []byte {
	c.env.t.Helper()
	var rdr io.Reader
	ct := ""
	switch b := body.(type) {
	case nil:
	case url.Values:
		rdr, ct = strings.NewReader(b.Encode()), "application/x-www-form-urlencoded"
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			c.env.t.Fatal(err)
		}
		rdr, ct = bytes.NewReader(raw), "application/json"
	}
	req, err := http.NewRequest(method, c.env.srv.URL+path, rdr)
	if err != nil {
		c.env.t.Fatal(err)
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.env.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if os.Getenv("ITEST_TRACE") != "" {
		c.env.t.Logf("%s %s -> %d %.150s", method, path, resp.StatusCode, out)
	}
	ok := resp.StatusCode < 500
	if len(want) > 0 {
		ok = false
		for _, w := range want {
			ok = ok || resp.StatusCode == w
		}
	}
	if !ok {
		c.env.t.Errorf("%s %s (%s): status %d, want %v: %.300s", method, path, c.name, resp.StatusCode, want, out)
	}
	return out
}

// post sends a JSON POST and decodes the object it returns.
func (c *itestClient) post(path string, body any, want ...int) map[string]any {
	c.env.t.Helper()
	raw := c.do(http.MethodPost, path, body, want...)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		c.env.t.Errorf("POST %s: not a JSON object: %.200s", path, raw)
	}
	return m
}

// rpc calls a Connect unary method with the JSON codec.
func (c *itestClient) rpc(procedure string, body any, want ...int) {
	c.env.t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	c.post(procedure, body, want...)
}

func (c *itestClient) upload(filename string, content []byte) map[string]any {
	c.env.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		c.env.t.Fatal(err)
	}
	_, _ = fw.Write(content)
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, c.env.srv.URL+"/api/activities/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		c.env.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		c.env.t.Fatalf("upload %s: %d %s", filename, resp.StatusCode, raw)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

// gpxRide renders a 20-minute 1 Hz track heading north-east from a start point.
func gpxRide(start time.Time) []byte {
	const lat, lon = 47.0, 8.0
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<gpx version="1.1" creator="itest" xmlns="http://www.topografix.com/GPX/1/1"
 xmlns:gpxtpx="http://www.garmin.com/xmlschemas/TrackPointExtension/v1">
<trk><name>Itest Ride</name><type>cycling</type><trkseg>`)
	for i := 0; i < 1200; i++ {
		fmt.Fprintf(&b, `<trkpt lat="%.6f" lon="%.6f"><ele>%.1f</ele><time>%s</time>`+
			`<extensions><power>%d</power><gpxtpx:TrackPointExtension><gpxtpx:hr>%d</gpxtpx:hr><gpxtpx:cad>85</gpxtpx:cad></gpxtpx:TrackPointExtension></extensions></trkpt>`,
			lat+float64(i)*0.00005, lon+float64(i)*0.00004, 300+float64(i%200)*0.5,
			start.Add(time.Duration(i)*time.Second).UTC().Format(time.RFC3339), 180+i%60, 120+i%40)
	}
	b.WriteString(`</trkseg></trk></gpx>`)
	return []byte(b.String())
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func TestAPI_EndToEnd(t *testing.T) {
	env := newITestEnv(t)
	adm := env.user("itadmin", domain.UserRoleAdmin)
	alice := env.user("italice", domain.UserRoleUser)
	bob := env.user("itbob", domain.UserRoleUser)
	anon := env.anon()

	// Public surface.
	for _, p := range []string{
		"/healthz", "/readyz", "/api/version", "/metrics", "/api/instance/features",
		"/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource",
		"/.well-known/nodeinfo", "/nodeinfo/2.1",
	} {
		anon.do(http.MethodGet, p, nil, http.StatusOK)
	}
	anon.do(http.MethodGet, "/api/overview", nil, http.StatusUnauthorized)
	anon.rpc("/cairn.v1.AuthService/GetLoginMethods", nil, http.StatusOK)
	anon.rpc("/cairn.v1.ActivityService/ListActivities", nil, http.StatusUnauthorized)

	// Uploads: alice twice (two rides), bob once on the same route.
	start := time.Date(2026, 5, 3, 7, 0, 0, 0, time.UTC)
	up := alice.upload("ride.gpx", gpxRide(start))
	actID := str(up, "activity_id")
	srcID := str(up, "source_id")
	if actID == "" || srcID == "" {
		t.Fatalf("upload response: %v", up)
	}
	again := alice.upload("ride-copy.gpx", gpxRide(start))
	if str(again, "activity_id") != actID {
		t.Errorf("re-upload of the same file should dedup, got %v", again)
	}
	up2 := alice.upload("ride2.gpx", gpxRide(start.Add(48*time.Hour)))
	act2, src2 := str(up2, "activity_id"), str(up2, "source_id")
	bobAct := str(bob.upload("bob.gpx", gpxRide(start.Add(24*time.Hour))), "activity_id")

	manual := alice.post("/api/activities/manual", map[string]any{
		"type": "run", "title": "Track session", "start_time": start.Add(72 * time.Hour).Format(time.RFC3339),
		"elapsed_duration_s": 3600, "moving_duration_s": 3500, "distance_m": 10000, "avg_heart_rate_bpm": 150,
	}, http.StatusOK, http.StatusCreated)
	manualID := str(manual, "activity_id")

	// Owner reads of the activity.
	for _, p := range []string{
		"", "/projected", "/laps", "/zones", "/similar", "/merged-stream", "/field-sources", "/manage",
		"/segment-efforts", "/kudos", "/comments", "/attachments", "/shares", "/export/options",
		"/export.gpx", "/export.tcx", "/export.fit", "/map.png", "/sources/" + srcID + "/parsed",
	} {
		alice.do(http.MethodGet, "/api/activities/"+actID+p, nil)
	}
	alice.do(http.MethodGet, "/api/activities/"+actID+"/sources/"+srcID+"/download", nil, http.StatusServiceUnavailable)
	alice.do(http.MethodGet, "/api/activities/"+actID+"/export.gpx", nil, http.StatusOK)
	alice.do(http.MethodGet, "/api/activities/"+actID+"/map.png", nil, http.StatusOK)

	// Owner lists and dashboards.
	for _, p := range []string{
		"/api/overview", "/api/stats", "/api/records", "/api/analysis", "/api/activities/feed",
		"/api/activities/heatmap", "/api/feed/home", "/api/feed/following",
		"/api/best-efforts/history?type=ride&metric=power&window_kind=duration&window_value=60",
		"/api/review-queue", "/api/quota", "/api/health?type=RestingHR&days=30", "/api/segments", "/api/athlete/metrics",
		"/api/settings/merge-policy", "/api/profile/visibility", "/api/profile/privacy-zones",
		"/api/notifications/preferences", "/api/notifications/quiet-hours", "/api/notifications/webhooks",
		"/api/notifications/deliveries", "/api/connections", "/api/webhook-endpoints", "/api/pats", "/api/blocks", "/api/clubs", "/api/profiles/italice",
	} {
		alice.do(http.MethodGet, p, nil, http.StatusOK)
	}

	// Activity mutations.
	alice.do(http.MethodPost, "/api/activities/"+actID+"/recompute", nil)
	alice.do(http.MethodPost, "/api/activities/"+actID+"/recompute-derived", nil)
	alice.do(http.MethodPost, "/api/activities/"+actID+"/reviewed", nil)
	alice.do(http.MethodPost, "/api/activities/"+actID+"/map/regenerate", nil)
	alice.do(http.MethodPut, "/api/activities/"+actID+"/field-source", map[string]any{"field": "title", "source_id": srcID})
	alice.do(http.MethodDelete, "/api/activities/"+actID+"/field-source/title", nil)
	alice.do(http.MethodPost, "/api/activities/"+manualID+"/source-from", map[string]any{"from_activity_id": act2, "groups": []string{"gps_track"}})
	alice.do(http.MethodPost, "/api/athlete/recalculate", nil)
	alice.do(http.MethodPut, "/api/athlete/metrics", map[string]any{"key": "ftp_watts", "effective_date": "2026-01-01", "value": 250}, http.StatusOK, http.StatusCreated)
	alice.do(http.MethodPut, "/api/athlete/metrics", map[string]any{"key": "max_hr", "effective_date": "2026-01-01", "value": 190}, http.StatusOK, http.StatusCreated)
	alice.do(http.MethodGet, "/api/activities/"+actID+"/zones", nil, http.StatusOK)

	// Connect-RPC owner surface.
	alice.rpc("/cairn.v1.ActivityService/ListActivities", nil, http.StatusOK)
	alice.rpc("/cairn.v1.ActivityService/GetActivity", map[string]any{"id": actID}, http.StatusOK)
	alice.rpc("/cairn.v1.ActivityService/GetActivityStream", map[string]any{"activity_id": actID})
	alice.rpc("/cairn.v1.ActivityService/ListBestEfforts", nil)
	for _, id := range []string{actID, act2, manualID} {
		alice.rpc("/cairn.v1.ActivityService/UpdateActivity", map[string]any{
			"id": id, "field_mask": []string{"title", "privacy", "tags"}, "title": "Renamed", "privacy": "PRIVACY_PUBLIC", "tags": []string{"itest"},
		}, http.StatusOK)
	}
	bob.rpc("/cairn.v1.ActivityService/UpdateActivity", map[string]any{"id": bobAct, "field_mask": []string{"privacy"}, "privacy": "PRIVACY_FOLLOWERS"}, http.StatusOK)
	bob.rpc("/cairn.v1.ActivityService/UpdateActivity", map[string]any{"id": actID, "field_mask": []string{"title"}, "title": "hijack"}, http.StatusNotFound, http.StatusForbidden, http.StatusUnauthorized)
	alice.rpc("/cairn.v1.AuthService/GetCurrentUser", nil, http.StatusOK)
	alice.rpc("/cairn.v1.NotificationService/ListNotifications", nil, http.StatusOK)
	alice.rpc("/cairn.v1.NotificationService/MarkAllNotificationsRead", nil)
	alice.rpc("/cairn.v1.AdminService/ListUsers", nil, http.StatusForbidden)

	// Privacy, profile and settings.
	alice.do(http.MethodPut, "/api/profile/public", map[string]any{"public": true}, http.StatusOK)
	alice.do(http.MethodPut, "/api/profile/visibility", map[string]any{"policy": map[string][]string{"followers": {"summary", "map", "location", "hr", "power", "social", "segments", "best_efforts", "zones"}, "public": {"summary", "social"}}}, http.StatusOK)
	zone := alice.post("/api/profile/privacy-zones", map[string]any{"label": "home", "lat": 47.0, "lng": 8.0, "radius_m": 200}, http.StatusOK, http.StatusCreated)
	alice.do(http.MethodGet, "/api/activities/"+actID+"/projected", nil, http.StatusOK)
	if id := str(zone, "id"); id != "" {
		alice.do(http.MethodDelete, "/api/profile/privacy-zones/"+id, nil)
	}
	alice.do(http.MethodPut, "/api/notifications/preferences", map[string]any{"preferences": []any{}})
	alice.do(http.MethodPut, "/api/notifications/quiet-hours", map[string]any{"enabled": true, "start": "22:00", "end": "06:00", "timezone": "UTC"})
	alice.do(http.MethodPut, "/api/settings/merge-policy", map[string]any{"policy": map[string]any{}})
	alice.do(http.MethodPut, "/api/profile/federation", map[string]any{"enabled": true})

	// PATs: create, use, revoke.
	pat := alice.post("/api/pats", map[string]any{"name": "cli", "expires_in_days": 7}, http.StatusCreated)
	cli := env.anon()
	cli.bearer, cli.name = str(pat, "token"), "italice-pat"
	cli.do(http.MethodGet, "/api/overview", nil, http.StatusOK)
	cli.rpc("/cairn.v1.ActivityService/ListActivities", nil, http.StatusOK)
	alice.do(http.MethodDelete, "/api/pats/"+str(pat, "id"), nil, http.StatusOK)
	cli.do(http.MethodGet, "/api/overview", nil, http.StatusUnauthorized)

	// Social: follow, kudos, comments, feed, blocks, reports.
	bobID, aliceID := bob.userID.String(), alice.userID.String()
	bob.do(http.MethodPost, "/api/users/"+aliceID+"/follow", nil, http.StatusOK)
	alice.do(http.MethodPost, "/api/users/"+bobID+"/follow", nil, http.StatusOK)
	for _, p := range []string{"/followers", "/following", "/follow-state"} {
		bob.do(http.MethodGet, "/api/users/"+aliceID+p, nil, http.StatusOK)
	}
	bob.do(http.MethodGet, "/api/activities/"+actID+"/projected", nil, http.StatusOK)
	bob.do(http.MethodGet, "/api/feed/following", nil, http.StatusOK)
	bob.do(http.MethodGet, "/api/profiles/italice", nil, http.StatusOK)
	bob.do(http.MethodPost, "/api/activities/"+actID+"/kudos", nil, http.StatusOK)
	bob.do(http.MethodGet, "/api/activities/"+actID+"/kudos", nil, http.StatusOK)
	cm := bob.post("/api/activities/"+actID+"/comments", map[string]any{"body": "Nice ride!"}, http.StatusOK)
	alice.do(http.MethodGet, "/api/activities/"+actID+"/comments", nil, http.StatusOK)
	alice.rpc("/cairn.v1.NotificationService/ListNotifications", nil, http.StatusOK)
	bob.do(http.MethodDelete, "/api/activities/"+actID+"/kudos", nil, http.StatusOK)
	rep := alice.post("/api/reports", map[string]any{"target_kind": "comment", "target_id": str(cm, "id"), "reason": "spam"}, http.StatusOK, http.StatusCreated)
	bob.do(http.MethodDelete, "/api/comments/"+str(cm, "id"), nil)
	alice.do(http.MethodGet, "/api/activities/"+bobAct+"/projected", nil)

	// Share links.
	sh := alice.post("/api/activities/"+actID+"/share", nil, http.StatusOK)
	anon.do(http.MethodGet, "/api/shared/"+str(sh, "token"), nil, http.StatusOK)
	alice.do(http.MethodGet, "/api/activities/"+actID+"/shares", nil, http.StatusOK)
	alice.do(http.MethodDelete, "/api/shares/"+str(sh, "token"), nil, http.StatusOK, http.StatusNoContent)

	// Clubs.
	club := alice.post("/api/clubs", map[string]any{"name": "Itest Riders", "description": "d"}, http.StatusOK)
	slug := str(club, "slug")
	bob.do(http.MethodPost, "/api/clubs/"+slug+"/join", nil, http.StatusOK)
	for _, p := range []string{"", "/members", "/feed"} {
		bob.do(http.MethodGet, "/api/clubs/"+slug+p, nil, http.StatusOK)
	}
	bob.do(http.MethodDelete, "/api/clubs/"+slug+"/leave", nil, http.StatusOK)

	// Federation (read side + local follow of a remote handle that does not resolve).
	for _, p := range []string{
		"/.well-known/webfinger?resource=acct:italice@" + strings.Replace(strings.TrimPrefix(env.srv.URL, "http://"), "127.0.0.1", "localhost", 1),
		"/users/italice", "/users/italice/outbox", "/users/italice/followers", "/users/italice/following",
	} {
		anon.do(http.MethodGet, p, nil)
	}
	alice.do(http.MethodPost, "/api/federation/follow", map[string]any{"handle": "nobody@127.0.0.1:1"})

	itestFederation(t, env, alice, actID)

	// Blocks.
	alice.do(http.MethodPost, "/api/users/"+bobID+"/block", nil, http.StatusOK)
	bob.do(http.MethodGet, "/api/activities/"+actID+"/projected", nil)
	alice.do(http.MethodDelete, "/api/users/"+bobID+"/block", nil, http.StatusOK)

	// Admin surface.
	for _, p := range []string{
		"/api/admin/invites", "/api/admin/reports", "/api/admin/worker-enrollments", "/api/admin/workers",
		"/api/admin/dlq", "/api/admin/federation/blocks", "/api/admin/instance/merge-defaults",
	} {
		adm.do(http.MethodGet, p, nil)
		bob.do(http.MethodGet, p, nil, http.StatusForbidden, http.StatusUnauthorized, http.StatusServiceUnavailable)
	}
	// Admin user management + audit log (REST and RPC twins).
	created := adm.post("/api/admin/users", map[string]any{"username": "dana", "email": "dana@example.com", "display_name": "Dana", "role": "user"}, http.StatusCreated)
	dana, _ := created["user"].(map[string]any)
	danaID := str(dana, "id")
	if danaID == "" || created["invite"] == nil {
		t.Fatalf("create user without password must mint an invite: %v", created)
	}
	adm.do(http.MethodGet, "/api/admin/users?limit=5", nil, http.StatusOK)
	adm.do(http.MethodGet, "/api/admin/users/"+danaID, nil, http.StatusOK)
	adm.do(http.MethodPut, "/api/admin/users/"+danaID, map[string]any{"display_name": "Caroline", "timezone": "Europe/Berlin"}, http.StatusOK)
	adm.do(http.MethodPut, "/api/admin/users/"+danaID+"/status", map[string]any{"status": "suspended", "reason": "test"}, http.StatusOK)
	adm.do(http.MethodPut, "/api/admin/users/"+danaID+"/status", map[string]any{"status": "active"}, http.StatusOK)
	adm.do(http.MethodPost, "/api/admin/users/"+danaID+"/reset-password", nil, http.StatusOK)
	adm.do(http.MethodPost, "/api/admin/users/"+danaID+"/revoke-credentials", map[string]any{"sessions": true}, http.StatusOK)
	adm.rpc("/cairn.v1.AdminService/GetUser", map[string]any{"id": danaID}, http.StatusOK)
	adm.rpc("/cairn.v1.AdminService/ChangeUserRole", map[string]any{"id": danaID, "role": "USER_ROLE_ADMIN"}, http.StatusOK)
	adm.rpc("/cairn.v1.AdminService/ListAuditEntries", map[string]any{"resourceId": danaID}, http.StatusOK)
	bob.rpc("/cairn.v1.AdminService/GetUser", map[string]any{"id": danaID}, http.StatusForbidden, http.StatusUnauthorized)
	adm.do(http.MethodDelete, "/api/admin/users/"+danaID+"?confirm_username=wrong", nil, http.StatusBadRequest)
	adm.do(http.MethodDelete, "/api/admin/users/"+danaID+"?confirm_username=dana&hard=true", nil, http.StatusOK)
	var trail struct {
		Entries []map[string]any `json:"entries"`
	}
	_ = json.Unmarshal(adm.do(http.MethodGet, "/api/admin/audit?resource_id="+danaID, nil, http.StatusOK), &trail)
	if len(trail.Entries) < 5 {
		t.Fatalf("audit trail for dana = %d entries", len(trail.Entries))
	}
	adm.do(http.MethodPost, "/api/admin/reports/"+str(rep, "id")+"/resolve", map[string]any{"status": "resolved"})
	adm.do(http.MethodPost, "/api/admin/activities/"+bobAct+"/hide", map[string]any{"hidden": true})
	adm.do(http.MethodPost, "/api/admin/federation/blocks", map[string]any{"domain": "evil.example", "reason": "spam"})
	adm.do(http.MethodDelete, "/api/admin/federation/blocks/evil.example", nil)
	adm.do(http.MethodPut, "/api/admin/users/"+bobID+"/role", map[string]any{"role": "moderator"})
	adm.do(http.MethodPut, "/api/admin/users/"+bobID+"/quota", map[string]any{"max_activities": 100})
	adm.do(http.MethodPut, "/api/admin/instance/merge-defaults", map[string]any{"policy": map[string]any{}})
	adm.do(http.MethodPost, "/api/admin/geocode/backfill", nil, http.StatusServiceUnavailable)
	adm.rpc("/cairn.v1.AdminService/ListUsers", nil, http.StatusOK)
	adm.rpc("/cairn.v1.AdminService/ListOIDCClients", nil, http.StatusOK)
	// Instance settings, health and outbound email on both surfaces.
	adm.do(http.MethodGet, "/api/admin/instance/settings", nil, http.StatusOK)
	adm.do(http.MethodPut, "/api/admin/instance/settings", map[string]any{"instance_name": "IT Cairn", "primary_color": "#112233"}, http.StatusOK)
	adm.do(http.MethodPut, "/api/admin/instance/settings", map[string]any{"primary_color": "red"}, http.StatusBadRequest)
	adm.rpc("/cairn.v1.AdminService/GetInstanceSettings", nil, http.StatusOK)
	adm.rpc("/cairn.v1.AdminService/UpdateInstanceSettings", map[string]any{"fieldMask": []string{"instance_name"}, "settings": map[string]any{"instanceName": "Cairn"}}, http.StatusOK)
	adm.do(http.MethodGet, "/api/admin/health", nil, http.StatusOK)
	adm.rpc("/cairn.v1.AdminService/GetSystemHealth", nil, http.StatusOK)
	bob.do(http.MethodGet, "/api/admin/health", nil, http.StatusForbidden)
	adm.rpc("/cairn.v1.AdminService/SendTestEmail", map[string]any{"toAddress": "itadmin@example.com"}, http.StatusOK, http.StatusServiceUnavailable)
	adm.rpc("/cairn.v1.AdminService/ListInvites", map[string]any{"includeUsed": true}, http.StatusOK)
	// Jobs, dead letters and orphaned sources.
	adm.do(http.MethodGet, "/api/admin/jobs?limit=10", nil, http.StatusOK)
	adm.do(http.MethodGet, "/api/admin/jobs/"+uuid.NewString(), nil, http.StatusNotFound)
	adm.do(http.MethodGet, "/api/admin/sources/orphaned", nil, http.StatusOK)
	adm.rpc("/cairn.v1.AdminService/ListJobs", map[string]any{}, http.StatusOK)
	adm.rpc("/cairn.v1.AdminService/ListDeadLetterJobs", map[string]any{}, http.StatusOK)
	adm.rpc("/cairn.v1.AdminService/ListOrphanedSources", map[string]any{}, http.StatusOK)
	adm.rpc("/cairn.v1.AdminService/GetJob", map[string]any{"id": uuid.NewString()}, http.StatusNotFound)

	// Invite → self-service signup → login of the new account.
	inv := adm.post("/api/admin/invites", map[string]any{"role": "user", "expires_in_days": 3}, http.StatusCreated)
	carol := env.anon()
	carol.name = "itcarol"
	carol.do(http.MethodPost, "/auth/invite/redeem", map[string]any{
		"code": str(inv, "code"), "username": "itcarol", "email": "itcarol@example.com",
		"password": "another good password", "display_name": "Carol",
	}, http.StatusOK)
	carol.do(http.MethodGet, "/api/overview", nil, http.StatusOK)
	carol.do(http.MethodPost, "/auth/invite/redeem", map[string]any{"code": str(inv, "code"), "username": "x", "email": "x@example.com", "password": "another good password"}, http.StatusBadRequest)
	adm.do(http.MethodPost, "/api/admin/invites/"+str(inv, "id")+"/resend", nil, http.StatusBadRequest, http.StatusServiceUnavailable)
	adm.do(http.MethodPost, "/api/admin/invites/"+str(inv, "id")+"/revoke", nil)
	adm.rpc("/cairn.v1.AdminService/RevokeInvite", map[string]any{"id": str(inv, "id")}, http.StatusOK)
	adm.rpc("/cairn.v1.AdminService/CreateInvite", map[string]any{"assignedRole": "USER_ROLE_USER"}, http.StatusOK)

	// Password recovery and login failures never leak account existence.
	anon.do(http.MethodPost, "/auth/password/forgot", map[string]any{"email": "italice@example.com"}, http.StatusOK)
	anon.do(http.MethodPost, "/auth/password/forgot", map[string]any{"email": "nobody@example.com"}, http.StatusOK)
	anon.do(http.MethodPost, "/auth/password/reset", map[string]any{"code": "bogus", "password": "long enough password"}, http.StatusBadRequest)
	anon.do(http.MethodGet, "/auth/email/verify?code=bogus", nil, http.StatusSeeOther)
	bad := env.anon()
	resp, err := bad.http.PostForm(env.srv.URL+"/auth/password", url.Values{"identifier": {"italice"}, "password": {"wrong"}, "redirect": {"//evil.example"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login?error=") {
		t.Errorf("failed login redirected to %q", loc)
	}

	// OAuth AS: dynamic registration and the authorize hand-off.
	reg := anon.post("/oauth/register", map[string]any{
		"client_name": "itest", "redirect_uris": []string{"http://127.0.0.1:9999/cb"}, "scope": "activities:read profile:read",
		"grant_types": []string{"authorization_code", "refresh_token"}, "token_endpoint_auth_method": "none",
	}, http.StatusCreated, http.StatusOK)
	if cid := str(reg, "client_id"); cid != "" {
		q := url.Values{"client_id": {cid}, "redirect_uri": {"http://127.0.0.1:9999/cb"}, "response_type": {"code"},
			"code_challenge": {"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"}, "code_challenge_method": {"S256"}, "state": {"s"}}
		anon.do(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil, http.StatusFound)
		q.Set("redirect_uri", "https://evil.example/cb")
		anon.do(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil, http.StatusBadRequest)
		alice.do(http.MethodGet, "/oauth/authorize/info?client_id="+cid+"&scope=activities:read", nil, http.StatusOK)
		dec := alice.post("/oauth/authorize/decision", map[string]any{
			"client_id": cid, "redirect_uri": "http://127.0.0.1:9999/cb", "scope": "activities:read", "state": "s",
			"code_challenge": "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", "code_challenge_method": "S256", "approve": true,
		}, http.StatusOK)
		if u, err := url.Parse(str(dec, "redirect_uri")); err == nil && u.Query().Get("code") != "" {
			tok := anon.post("/oauth/token", url.Values{
				"grant_type": {"authorization_code"}, "code": {u.Query().Get("code")}, "client_id": {cid},
				"redirect_uri": {"http://127.0.0.1:9999/cb"}, "code_verifier": {"dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"},
			}, http.StatusOK)
			oc := env.anon()
			oc.bearer, oc.name = str(tok, "access_token"), "oauth"
			oc.do(http.MethodGet, "/api/overview", nil, http.StatusOK)
			oc.rpc("/cairn.v1.ActivityService/ListActivities", nil, http.StatusOK)
			oc.post("/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"}, http.StatusOK)
			oc.do(http.MethodPost, "/mcp", map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
			oc.post("/mcp", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "ping"}, http.StatusOK)
			oc.post("/mcp", map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"}, http.StatusOK)
			for _, tool := range []map[string]any{
				{"name": "list_activities", "arguments": map[string]any{"limit": 5}},
				{"name": "get_activity", "arguments": map[string]any{"activity_id": actID}},
				{"name": "activity_stats", "arguments": map[string]any{}},
				{"name": "personal_records", "arguments": map[string]any{}},
				{"name": "profile", "arguments": map[string]any{}},
				{"name": "nope", "arguments": map[string]any{}},
			} {
				oc.post("/mcp", map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": tool}, http.StatusOK)
			}
			oc.post("/mcp", map[string]any{"jsonrpc": "2.0", "id": 5, "method": "bogus"}, http.StatusOK)
			anon.do(http.MethodPost, "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, http.StatusUnauthorized)
			anon.do(http.MethodGet, "/mcp", nil)
			oc.do(http.MethodPost, "/api/activities/"+actID+"/reviewed", nil, http.StatusUnauthorized, http.StatusForbidden)
			anon.do(http.MethodPost, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {str(tok, "refresh_token")}, "client_id": {cid}}, http.StatusOK)
			// Authorized apps: the client shows up with live tokens, then revoking it ends them.
			var apps struct {
				Apps []map[string]any `json:"apps"`
			}
			_ = json.Unmarshal(alice.do(http.MethodGet, "/api/oauth/apps", nil, http.StatusOK), &apps)
			if len(apps.Apps) != 1 {
				t.Fatalf("authorized apps = %v", apps)
			}
			alice.rpc("/cairn.v1.AuthService/ListAuthorizedApps", map[string]any{}, http.StatusOK)
			alice.do(http.MethodDelete, "/api/oauth/apps/"+cid, nil, http.StatusOK)
			alice.do(http.MethodDelete, "/api/oauth/apps/"+cid, nil, http.StatusNotFound)
			oc.do(http.MethodGet, "/api/overview", nil, http.StatusUnauthorized)
			anon.do(http.MethodPost, "/oauth/revoke", url.Values{"token": {str(tok, "access_token")}, "client_id": {cid}})
		}
	}

	// Notification webhooks.
	wh := alice.post("/api/notifications/webhooks", map[string]any{
		"name": "hook", "url": "https://hooks.example/cairn", "event_types": []int{1, 2}, "min_severity": "info",
	}, http.StatusCreated)
	if id := str(wh, "id"); id != "" {
		alice.do(http.MethodPut, "/api/notifications/webhooks/"+id, map[string]any{"name": "hook2", "url": "https://hooks.example/cairn2", "enabled": false}, http.StatusOK)
		alice.do(http.MethodPost, "/api/notifications/webhooks/"+id+"/rotate-secret", nil, http.StatusOK)
		alice.do(http.MethodPost, "/api/notifications/webhooks/"+id+"/test", nil, http.StatusOK, http.StatusBadGateway, http.StatusBadRequest)
		alice.do(http.MethodGet, "/api/notifications/webhooks", nil, http.StatusOK)
		bob.do(http.MethodDelete, "/api/notifications/webhooks/"+id, nil, http.StatusNotFound, http.StatusForbidden)
		alice.do(http.MethodDelete, "/api/notifications/webhooks/"+id, nil, http.StatusOK, http.StatusNoContent)
	}
	alice.do(http.MethodPost, "/api/notifications/webhooks", map[string]any{"name": "ssrf", "url": "http://127.0.0.1/x"}, http.StatusBadRequest)

	// Worker enrollments: provider is required and format-checked, not limited to built-ins.
	for _, p := range []string{"", "polar.>", "*", "po.lar", "Polar", strings.Repeat("p", 49)} {
		msg := adm.do(http.MethodPost, "/api/admin/worker-enrollments", map[string]any{"name": "itest-worker", "provider": p}, http.StatusBadRequest)
		if want := "provider required — lowercase letters, digits, hyphens, underscores"; strings.TrimSpace(string(msg)) != want {
			t.Errorf("provider %q: body %q, want %q", p, msg, want)
		}
	}
	polar := adm.post("/api/admin/worker-enrollments", map[string]any{"name": "polar-worker", "provider": "polar"}, http.StatusCreated)
	if e, ok := polar["enrollment"].(map[string]any); !ok || str(e, "provider") != "polar" || str(polar, "token") == "" {
		t.Errorf("polar enrollment = %v", polar)
	}
	legacy := domain.WorkerEnrollment{
		ID: domain.WorkerEnrollmentID(uuid.New()), TokenHash: make([]byte, 32), Name: "legacy-worker",
		Version: 1, WorkerNamePattern: "*", PermissionTemplate: "standard", ExpiresAt: time.Now().Add(time.Hour),
	}
	if _, err := env.app.WorkerEnrollments.CreateEnrollment(context.Background(), legacy); err != nil {
		t.Fatalf("store enrollment without provider: %v", err)
	}
	enr := adm.post("/api/admin/worker-enrollments", map[string]any{"name": "itest-worker", "provider": "strava", "expires_in_hours": 24, "note": "n"}, http.StatusCreated)
	if e, ok := enr["enrollment"].(map[string]any); ok {
		id := str(e, "id")
		if str(e, "provider") != "strava" || e["reenroll_required"] != false {
			t.Errorf("created enrollment = %v", e)
		}
		var list struct {
			Enrollments []map[string]any `json:"enrollments"`
			Providers   []string         `json:"providers"`
		}
		if err := json.Unmarshal(adm.do(http.MethodGet, "/api/admin/worker-enrollments", nil, http.StatusOK), &list); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(list.Providers, []string{"garmin", "strava"}) {
			t.Errorf("providers = %v", list.Providers)
		}
		for _, le := range list.Enrollments {
			if want := str(le, "id") == legacy.ID.String(); le["reenroll_required"] != want {
				t.Errorf("enrollment %s: reenroll_required = %v, want %v", str(le, "id"), le["reenroll_required"], want)
			}
		}
		adm.do(http.MethodPost, "/api/admin/worker-enrollments/"+id+"/prolong", map[string]any{"expires_in_hours": 48})
		adm.do(http.MethodPost, "/api/admin/worker-enrollments/"+id+"/revoke", nil)
	}

	// Source management: merge two activities, split them again, detach.
	alice.do(http.MethodPut, "/api/activities/"+actID+"/field-source", map[string]any{"field": "title", "source_id": srcID}, http.StatusOK, http.StatusNoContent)
	alice.do(http.MethodPost, "/api/activities/"+manualID+"/source-from", map[string]any{"donor_id": act2, "groups": []string{"gps_track", "heart_rate", "distance"}})
	merged := alice.post("/api/activities/merge", map[string]any{"activity_a": actID, "activity_b": act2})
	_ = merged
	alice.do(http.MethodGet, "/api/activities/"+actID+"/field-sources", nil)
	for _, a := range []string{actID, act2} {
		for _, src := range []string{srcID, src2} {
			alice.do(http.MethodPost, "/api/activities/"+a+"/split", map[string]any{"source_id": src})
		}
	}
	alice.do(http.MethodPost, "/api/activities/"+actID+"/sources/"+srcID+"/reimport", map[string]any{"mode": "replace"}, http.StatusServiceUnavailable)
	alice.do(http.MethodPost, "/api/activities/"+actID+"/sources/"+srcID+"/detach", map[string]any{"reason": "duplicate"})
	// DELETE soft-deletes through the shared use case: gone from reads, a
	// second delete is 404, and another user's activity is invisible.
	alice.do(http.MethodDelete, "/api/activities/"+act2, nil)
	alice.do(http.MethodGet, "/api/activities/"+act2+"/manage", nil, http.StatusNotFound)
	alice.do(http.MethodDelete, "/api/activities/"+act2, nil, http.StatusNotFound)
	bob.do(http.MethodDelete, "/api/activities/"+actID, nil, http.StatusNotFound)

	itestConnections(t, alice)

	// Passkey ceremonies start (finishing needs an authenticator).
	alice.do(http.MethodPost, "/auth/webauthn/register/start", nil)
	anon.do(http.MethodPost, "/auth/webauthn/login/start", nil)
	alice.do(http.MethodGet, "/api/passkeys", nil)
	alice.do(http.MethodPost, "/auth/email/verify/send", nil)

	// Deletions last.
	alice.do(http.MethodPost, "/auth/logout", nil)
	alice.do(http.MethodGet, "/api/overview", nil, http.StatusUnauthorized)
}

// itestFederation plays a remote ActivityPub server against alice: outbound follow via WebFinger,
// then signed inbound Follow, Create, Like, reply, Undo and Delete.
func itestFederation(t *testing.T, env *itestEnv, alice *itestClient, actID string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var actor string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/.well-known/webfinger":
			w.Header().Set("Content-Type", "application/jrd+json")
			_ = json.NewEncoder(w).Encode(map[string]any{"links": []any{
				map[string]any{"rel": "self", "type": "application/activity+json", "href": actor},
			}})
		case r.URL.Path == "/users/rbob":
			w.Header().Set("Content-Type", "application/activity+json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"@context": []any{"https://www.w3.org/ns/activitystreams"}, "type": "Person", "id": actor,
				"preferredUsername": "rbob", "inbox": actor + "/inbox",
				"publicKey": map[string]any{"id": actor + "#main-key", "owner": actor, "publicKeyPem": pubKeyPEM(t, key)},
			})
		case r.URL.Path == "/users/rbob/inbox" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(remote.Close)
	actor = remote.URL + "/users/rbob"
	host := strings.TrimPrefix(remote.URL, "http://")

	alice.do(http.MethodPost, "/api/federation/follow", map[string]any{"handle": "@rbob@" + host}, http.StatusOK)

	base := strings.Replace(env.srv.URL, "127.0.0.1", "localhost", 1)
	aliceActor := base + "/users/italice"
	objURL := aliceActor + "/activities/" + actID
	send := func(activity map[string]any, want ...int) {
		t.Helper()
		activity["@context"] = "https://www.w3.org/ns/activitystreams"
		activity["actor"] = actor
		body, _ := json.Marshal(activity)
		req, _ := http.NewRequest(http.MethodPost, env.srv.URL+"/users/italice/inbox", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/activity+json")
		if err := httpsig.Sign(req, actor+"#main-key", key, body); err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if !slices.Contains(want, resp.StatusCode) {
			t.Errorf("inbox %v: status %d, want %v", activity["type"], resp.StatusCode, want)
		}
	}
	id := func(s string) string { return actor + "/" + s + "/" + uuid.NewString() }
	follow := map[string]any{"id": id("follows"), "type": "Follow", "object": aliceActor}
	send(follow, http.StatusAccepted)
	send(map[string]any{"id": id("accepts"), "type": "Accept", "object": map[string]any{"type": "Follow", "actor": aliceActor, "object": actor}}, http.StatusAccepted)
	note := buildActivityCreate(remote.URL, actor, actor+"/activities/"+uuid.NewString(), domain.Activity{
		ID: domain.ActivityID(uuid.New()), Type: domain.ActivityTypeRun, Title: "Remote run",
		StartTime: time.Now().UTC().Add(-time.Hour), MovingDuration: 30 * time.Minute, Privacy: domain.PrivacyPublic,
	})
	send(map[string]any{"id": id("creates"), "type": "Create", "object": note["object"]}, http.StatusAccepted)
	like := map[string]any{"id": id("likes"), "type": "Like", "object": objURL}
	send(like, http.StatusAccepted)
	send(like, http.StatusAccepted)
	send(map[string]any{"id": id("creates"), "type": "Create", "object": map[string]any{
		"id": id("notes"), "type": "Note", "inReplyTo": objURL, "content": "<p>Great ride!</p>",
	}}, http.StatusAccepted)
	alice.do(http.MethodGet, "/api/activities/"+actID+"/comments", nil, http.StatusOK)
	alice.do(http.MethodGet, "/api/feed/home", nil, http.StatusOK)
	send(map[string]any{"id": id("undos"), "type": "Undo", "object": map[string]any{"type": "Like", "object": objURL}}, http.StatusAccepted)
	send(map[string]any{"id": id("undos"), "type": "Undo", "object": map[string]any{"type": "Follow", "object": aliceActor}}, http.StatusAccepted)
	send(map[string]any{"id": id("deletes"), "type": "Delete", "object": actor}, http.StatusAccepted)
	send(map[string]any{"id": id("follows"), "type": "Follow", "object": base + "/users/nobody"}, http.StatusNotFound, http.StatusBadRequest)

	// Unsigned and badly signed requests are refused.
	resp, err := http.Post(env.srv.URL+"/users/italice/inbox", "application/activity+json", strings.NewReader(`{"type":"Follow"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unsigned inbox post: %d", resp.StatusCode)
	}
}

// itestConnections links a Strava account through a fake token endpoint and a Garmin credential account.
func itestConnections(t *testing.T, alice *itestClient) {
	t.Helper()
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.PostForm.Get("code") != "good" {
			http.Error(w, "bad code", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":21600,"token_type":"Bearer","scope":"read","athlete":{"id":4242}}`))
	}))
	t.Cleanup(tokens.Close)
	orig := providerRegistry["strava"]
	patched := orig
	patched.TokenURL = tokens.URL
	providerRegistry["strava"] = patched
	t.Cleanup(func() { providerRegistry["strava"] = orig })

	conn := alice.post("/api/connections", map[string]any{"provider": "strava", "label": "My Strava", "client_id": "123", "client_secret": "s3cret"}, http.StatusCreated)
	id := str(conn, "id")
	alice.do(http.MethodPut, "/api/connections/"+id, map[string]any{"label": "Strava", "client_id": "123", "client_secret": "s3cret2"}, http.StatusOK, http.StatusNoContent)
	alice.do(http.MethodGet, "/api/connections", nil, http.StatusOK)
	alice.do(http.MethodGet, "/api/webhook-endpoints", nil, http.StatusOK)
	alice.do(http.MethodGet, "/auth/oauth/strava/connect", nil, http.StatusFound)
	alice.do(http.MethodGet, "/auth/oauth/strava/connect?connection="+id, nil, http.StatusFound)
	var state string
	for _, c := range alice.http.Jar.Cookies(mustURL(t, alice.env.srv.URL+"/auth/oauth/strava/callback")) {
		if c.Name == oauthStateCookie {
			state = c.Value
		}
	}
	alice.do(http.MethodGet, "/auth/oauth/strava/callback?state=wrong&code=good", nil, http.StatusBadRequest)
	alice.do(http.MethodGet, "/auth/oauth/strava/callback?state="+state+"&code=bad", nil, http.StatusFound)
	alice.do(http.MethodGet, "/auth/oauth/strava/callback?state="+state+"&code=good", nil, http.StatusFound)
	alice.do(http.MethodGet, "/auth/oauth/garmin/connect", nil, http.StatusBadRequest)
	alice.do(http.MethodGet, "/api/connections", nil, http.StatusOK)

	alice.do(http.MethodPost, "/api/connections", map[string]any{"provider": "garmin", "client_id": "me@example.com"}, http.StatusBadRequest)
	alice.do(http.MethodPost, "/api/connections", map[string]any{"provider": "nope", "client_id": "x"}, http.StatusBadRequest)
	g := alice.post("/api/connections", map[string]any{"provider": "garmin", "client_id": "me@example.com", "client_secret": "pw"}, http.StatusCreated)
	alice.do(http.MethodGet, "/api/connections", nil, http.StatusOK)
	alice.do(http.MethodDelete, "/api/connections/"+str(g, "id"), nil)
	alice.do(http.MethodDelete, "/api/connections/"+id, nil)
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
