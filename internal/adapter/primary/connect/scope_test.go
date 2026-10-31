package connect

import (
	"testing"

	connectrpc "connectrpc.com/connect/v2"
)

func TestIsMutatingProcedure(t *testing.T) {
	reads := []string{
		"/cairn.v1.ActivityService/GetActivity",
		"/cairn.v1.ActivityService/ListActivities",
		"/cairn.v1.ActivityService/GetActivityStream",
		"/cairn.v1.SegmentService/SearchSegments",
		"/cairn.v1.MetricService/ExportMetrics",
		"/cairn.v1.MetricsService/QueryMetricSeries",
	}
	for _, p := range reads {
		if isMutatingProcedure(p) {
			t.Errorf("expected read (non-mutating): %s", p)
		}
	}
	writes := []string{
		"/cairn.v1.ActivityService/UpdateActivity",
		"/cairn.v1.ActivityService/DeleteActivity",
		"/cairn.v1.ActivityService/CreateManualActivity",
		"/cairn.v1.EngagementService/PostComment",
	}
	for _, p := range writes {
		if !isMutatingProcedure(p) {
			t.Errorf("expected mutating (write): %s", p)
		}
	}
}

func TestScopeCheckFamilies(t *testing.T) {
	cases := []struct {
		proc   string
		scopes []string
		want   connectrpc.Code // 0 = allowed
	}{
		{"/cairn.v1.ActivityService/GetActivity", []string{"activities:read"}, 0},
		{"/cairn.v1.ActivityService/UpdateActivity", []string{"activities:read"}, connectrpc.CodePermissionDenied},
		{"/cairn.v1.ActivityService/UpdateActivity", []string{"activities:write"}, 0},
		{"/cairn.v1.ActivityService/GetActivity", []string{"activities:write"}, 0},
		{"/cairn.v1.GearService/ListGear", []string{"activities:read"}, connectrpc.CodePermissionDenied},
		{"/cairn.v1.GearService/ListGear", []string{"gear:read"}, 0},
		{"/cairn.v1.NotificationService/MarkNotificationRead", []string{"notifications:read"}, connectrpc.CodePermissionDenied},
		{"/cairn.v1.NotificationService/MarkNotificationRead", []string{"notifications:write"}, 0},
		{"/cairn.v1.AuthService/GetCurrentUser", nil, 0},
		{"/cairn.v1.AuthService/GetQuota", []string{"gear:read"}, 0},
		{"/cairn.v1.AuthService/GetInstanceInfo", nil, 0},
		{"/cairn.v1.AuthService/UpdateUserPreferences", []string{"profile:read"}, connectrpc.CodePermissionDenied},
		{"/cairn.v1.AuthService/UpdateUserPreferences", []string{"profile:write"}, 0},
		{"/cairn.v1.AuthService/ListLinkedIdentities", []string{"profile:read"}, 0},
		{"/cairn.v1.AuthService/CreatePersonalAccessToken", []string{"profile:write", "activities:write"}, connectrpc.CodePermissionDenied},
		{"/cairn.v1.AuthService/Logout", []string{"profile:write"}, connectrpc.CodePermissionDenied},
		{"/cairn.v1.AdminService/ListUsers", []string{"activities:write"}, connectrpc.CodePermissionDenied},
		{"/cairn.v1.SettingsService/GetSettings", []string{"profile:read"}, 0},
		{"/cairn.v1.MetricsService/UpsertMetric", []string{"training:write"}, 0},
		{"/cairn.v1.Unknown/Get", []string{"activities:read"}, connectrpc.CodePermissionDenied},
	}
	for _, c := range cases {
		err := scopeCheck(c.proc, c.scopes)
		switch {
		case c.want == 0 && err != nil:
			t.Errorf("%s with %v: got %v, want allowed", c.proc, c.scopes, err)
		case c.want != 0 && connectrpc.CodeOf(err) != c.want:
			t.Errorf("%s with %v: got %v, want %v", c.proc, c.scopes, err, c.want)
		}
	}
}
