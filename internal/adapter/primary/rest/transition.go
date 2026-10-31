package rest

// Shared helpers whose users are still being moved out of cmd/server. Delete
// this file once every handler group lives here.
var _ = []any{
	actionErrorOf, actor, clientIP, decodeJSONBody, decodeOptionalJSONBody, hostOnly,
	parseIntQuery, projectActivityJSON, requireJSON, truncateUA, userCardsJSON,
}
