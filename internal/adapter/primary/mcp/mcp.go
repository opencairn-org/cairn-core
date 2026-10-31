// Package mcp is an OAuth-protected Model Context Protocol endpoint that lets
// AI agents read a single user's Cairn data. Read-only by design.
//
// Transport: Streamable HTTP. The client POSTs JSON-RPC 2.0 to /mcp and gets a
// single JSON-RPC response (no SSE channel: the tools are request/response).
// Auth is an OAuth 2.1 bearer access token; a missing or invalid one gets a
// 401 whose WWW-Authenticate points at the protected-resource metadata (RFC
// 9728), which is how MCP clients discover the authorization server.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/oauthserver"
	"github.com/opencairn-org/cairn-core/internal/usecase/owndata"
)

const mcpProtocolVersion = "2025-06-18"

// Server serves the MCP endpoint. Tools run in-process, scoped to the token's
// user and gated on its scopes.
type Server struct {
	Tokens *oauthserver.Server
	Data   *owndata.Reader
	// BaseURL is the public base URL, without a trailing slash.
	BaseURL string
}

// Mount registers the routes.
func (s *Server) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /mcp", s.handle)
	// MCP clients may probe with GET; tell them where to authenticate.
	mux.HandleFunc("GET /mcp", func(w http.ResponseWriter, _ *http.Request) { s.challenge(w) })
}

// ToolCount is the number of tools offered.
func ToolCount() int { return len(mcpTools) }

// ---------------------------------------------------------------------------
// JSON-RPC envelope
// ---------------------------------------------------------------------------

type jsonrpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	// Authenticate the bearer access token up front.
	at, ok := s.authToken(r)
	if !ok {
		s.challenge(w)
		return
	}

	var req jsonrpcReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, jsonrpcResp{JSONRPC: "2.0", Error: &jsonrpcError{Code: -32700, Message: "parse error"}})
		return
	}

	// Notifications (no id) get no body.
	isNotification := len(req.ID) == 0
	resp := jsonrpcResp{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "cairn", "version": "1"},
			"instructions":    "Read-only access to the authenticated user's Cairn activity data.",
		}
	case "notifications/initialized", "notifications/cancelled":
		w.WriteHeader(http.StatusAccepted)
		return
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": mcpToolList()}
	case "tools/call":
		result, rpcErr := s.callTool(r, at, req.Params)
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = result
		}
	default:
		resp.Error = &jsonrpcError{Code: -32601, Message: "method not found: " + req.Method}
	}

	if isNotification {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// challenge emits a 401 telling the client where to authenticate (RFC 9728).
func (s *Server) challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate",
		fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource"`, s.BaseURL))
	writeJSON(w, http.StatusUnauthorized, jsonrpcResp{JSONRPC: "2.0",
		Error: &jsonrpcError{Code: -32001, Message: "authentication required"}})
}

// authToken validates the OAuth bearer access token.
func (s *Server) authToken(r *http.Request) (domain.OAuthAccessToken, bool) {
	return s.Tokens.AccessToken(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
}

// ---------------------------------------------------------------------------
// Tools
// ---------------------------------------------------------------------------

type mcpTool struct {
	Name        string
	Description string
	Scope       string         // required OAuth scope
	InputSchema map[string]any // JSON Schema
}

var mcpTools = []mcpTool{
	{
		Name:        "list_activities",
		Description: "List the user's most recent activities (newest first).",
		Scope:       domain.ScopeActivitiesRead,
		InputSchema: objSchema(map[string]any{
			"limit": map[string]any{"type": "integer", "description": "Max activities to return (default 20, max 100)."},
		}, nil),
	},
	{
		Name:        "get_activity",
		Description: "Get one activity by id, including its summary metrics.",
		Scope:       domain.ScopeActivitiesRead,
		InputSchema: objSchema(map[string]any{
			"activity_id": map[string]any{"type": "string", "description": "The activity UUID."},
		}, []string{"activity_id"}),
	},
	{
		Name:        "activity_stats",
		Description: "Lifetime totals: activity count, distance, moving time, elevation gain.",
		Scope:       domain.ScopeActivitiesRead,
		InputSchema: objSchema(nil, nil),
	},
	{
		Name:        "personal_records",
		Description: "The user's current personal records (best efforts across distance/time windows).",
		Scope:       domain.ScopeActivitiesRead,
		InputSchema: objSchema(nil, nil),
	},
	{
		Name:        "profile",
		Description: "The user's profile and unit/locale preferences.",
		Scope:       domain.ScopeProfileRead,
		InputSchema: objSchema(nil, nil),
	},
}

func mcpToolList() []map[string]any {
	out := make([]map[string]any, 0, len(mcpTools))
	for _, t := range mcpTools {
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
		})
	}
	return out
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Server) callTool(r *http.Request, at domain.OAuthAccessToken, raw json.RawMessage) (any, *jsonrpcError) {
	var p toolCallParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &jsonrpcError{Code: -32602, Message: "invalid params"}
	}
	var tool *mcpTool
	for i := range mcpTools {
		if mcpTools[i].Name == p.Name {
			tool = &mcpTools[i]
			break
		}
	}
	if tool == nil {
		return nil, &jsonrpcError{Code: -32602, Message: "unknown tool: " + p.Name}
	}
	if !at.HasScope(tool.Scope) {
		return toolError(fmt.Sprintf("this token lacks the %q scope required by %s", tool.Scope, tool.Name)), nil
	}

	ctx := r.Context()
	uid := at.UserID
	switch tool.Name {
	case "list_activities":
		return s.toolListActivities(ctx, uid, p.Arguments)
	case "get_activity":
		return s.toolGetActivity(ctx, uid, p.Arguments)
	case "activity_stats":
		return s.toolActivityStats(ctx, uid)
	case "personal_records":
		return s.toolPersonalRecords(ctx, uid)
	case "profile":
		return s.toolProfile(ctx, uid)
	}
	return nil, &jsonrpcError{Code: -32601, Message: "tool not implemented"}
}

func (s *Server) toolListActivities(ctx context.Context, uid domain.UserID, raw json.RawMessage) (any, *jsonrpcError) {
	var args struct {
		Limit int `json:"limit"`
	}
	_ = json.Unmarshal(raw, &args)
	acts, err := s.Data.Recent(ctx, uid, args.Limit)
	if err != nil {
		return toolError("could not list activities"), nil
	}
	items := make([]map[string]any, 0, len(acts))
	for _, a := range acts {
		items = append(items, activitySummaryJSON(a))
	}
	return toolJSON(map[string]any{"activities": items, "count": len(items)})
}

func (s *Server) toolGetActivity(ctx context.Context, uid domain.UserID, raw json.RawMessage) (any, *jsonrpcError) {
	var args struct {
		ActivityID string `json:"activity_id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil || args.ActivityID == "" {
		return toolError("activity_id is required"), nil
	}
	id, err := domain.ParseUUID[domain.ActivityID](args.ActivityID)
	if err != nil {
		return toolError("invalid activity_id"), nil
	}
	a, err := s.Data.Activity(ctx, uid, id)
	if err != nil {
		return toolError("activity not found"), nil
	}
	return toolJSON(activityDetailJSON(a))
}

func (s *Server) toolActivityStats(ctx context.Context, uid domain.UserID) (any, *jsonrpcError) {
	t, err := s.Data.Totals(ctx, uid)
	if err != nil {
		return toolError("could not compute totals"), nil
	}
	return toolJSON(map[string]any{
		"activity_count":   t.Count,
		"distance_m":       t.DistanceM,
		"moving_time_s":    t.MovingS,
		"elevation_gain_m": t.ElevationGainM,
	})
}

func (s *Server) toolPersonalRecords(ctx context.Context, uid domain.UserID) (any, *jsonrpcError) {
	prs, err := s.Data.PersonalRecords(ctx, uid)
	if err != nil {
		return toolError("could not list personal records"), nil
	}
	items := make([]map[string]any, 0, len(prs))
	for _, pr := range prs {
		items = append(items, map[string]any{
			"activity_type":  string(pr.ActivityType),
			"metric":         string(pr.Metric),
			"window_kind":    string(pr.WindowKind),
			"window_value":   pr.WindowValue,
			"achieved_value": pr.AchievedValue,
			"activity_id":    pr.ActivityID.String(),
			"achieved_at":    pr.Timestamp.Format(time.RFC3339),
		})
	}
	return toolJSON(map[string]any{"personal_records": items, "count": len(items)})
}

func (s *Server) toolProfile(ctx context.Context, uid domain.UserID) (any, *jsonrpcError) {
	u, err := s.Data.Profile(ctx, uid)
	if err != nil {
		return toolError("could not load profile"), nil
	}
	return toolJSON(map[string]any{
		"username":     u.Username,
		"display_name": u.DisplayName,
		"email":        u.Email,
		"locale":       u.Locale,
		"timezone":     u.Timezone,
		"units":        string(u.Units),
	})
}

// ---------------------------------------------------------------------------
// Serialization helpers
// ---------------------------------------------------------------------------

func activitySummaryJSON(a domain.Activity) map[string]any {
	m := map[string]any{
		"id":         a.ID.String(),
		"type":       string(a.Type),
		"title":      a.Title,
		"start_time": a.StartTime.Format(time.RFC3339),
		"moving_s":   a.MovingDuration.Seconds(),
	}
	if a.Summary.DistanceM != nil {
		m["distance_m"] = *a.Summary.DistanceM
	}
	if a.Summary.ElevationGainM != nil {
		m["elevation_gain_m"] = *a.Summary.ElevationGainM
	}
	if a.Summary.AvgHeartRateBpm != nil {
		m["avg_hr_bpm"] = *a.Summary.AvgHeartRateBpm
	}
	return m
}

func activityDetailJSON(a domain.Activity) map[string]any {
	m := activitySummaryJSON(a)
	m["description"] = a.Description
	m["discipline"] = string(a.Discipline)
	m["timezone"] = a.Timezone
	m["elapsed_s"] = a.ElapsedDuration.Seconds()
	m["is_race"] = a.IsRace
	m["is_commute"] = a.IsCommute
	m["tags"] = a.Tags
	if a.Summary.AvgSpeedMps != nil {
		m["avg_speed_mps"] = *a.Summary.AvgSpeedMps
	}
	if a.Summary.AvgPowerW != nil {
		m["avg_power_w"] = *a.Summary.AvgPowerW
	}
	if a.StartPlace != "" {
		m["start_place"] = a.StartPlace
	}
	return m
}

// objSchema builds a JSON Schema object with the given properties + required.
func objSchema(props map[string]any, required []string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// toolJSON wraps a result value as MCP tool content (a single JSON text block).
func toolJSON(v any) (any, *jsonrpcError) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, &jsonrpcError{Code: -32603, Message: "internal error"}
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(b)}},
	}, nil
}

// toolError returns an MCP tool result flagged as an error (isError: true),
// which is how a tool reports a problem to the agent (vs. a protocol error).
func toolError(msg string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"isError": true,
	}
}
