package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// AuditRepo implements port.AuditRepo over the audit_entries hypertable.
type AuditRepo struct {
	pool *pgxpool.Pool
}

var _ port.AuditRepo = (*AuditRepo)(nil)

func NewAuditRepo(pool *pgxpool.Pool) *AuditRepo {
	return &AuditRepo{pool: pool}
}

func (r *AuditRepo) Record(ctx context.Context, e domain.AuditEntry) error {
	db := dbtx(ctx, r.pool)
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	id := e.ID.UUID()
	if id == uuid.Nil {
		id = uuid.New()
	}
	diff := map[string]map[string]string{}
	for _, c := range e.Diff {
		diff[c.Field] = map[string]string{"before": c.Before, "after": c.After}
	}
	diffJSON, _ := json.Marshal(diff)
	meta := e.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	metaJSON, _ := json.Marshal(meta)
	_, err := db.Exec(ctx,
		`INSERT INTO audit_entries (id, ts, actor_user_id, actor_username, actor_pat_id, ip_address, user_agent_summary,
		                            action, severity, resource_type, resource_id, diff, metadata)
		 VALUES ($1, $2, $3, NULLIF($4, ''), $5, NULLIF($6, '')::inet, NULLIF($7, ''), $8, $9, NULLIF($10, ''), NULLIF($11, ''), $12, $13)`,
		id, e.At, userIDParam(e.ActorUserID), e.ActorUsername, patIDParam(e.ActorPATID), e.IPAddress, e.UserAgent,
		e.Action, string(severityOrInfo(e.Severity)), e.ResourceType, e.ResourceID, diffJSON, metaJSON)
	if err != nil {
		return fmt.Errorf("record audit entry: %w", err)
	}
	return nil
}

func (r *AuditRepo) List(ctx context.Context, f domain.AuditFilter) ([]domain.AuditEntry, error) {
	db := dbtx(ctx, r.pool)
	where, args := auditWhere(f)
	limit, offset := f.Limit, max(0, f.Offset)
	if limit <= 0 {
		limit = 50
	}
	args = append(args, limit, offset)
	rows, err := db.Query(ctx,
		`SELECT id, ts, actor_user_id, COALESCE(actor_username, ''), actor_pat_id, COALESCE(host(ip_address)::text, ''),
		        COALESCE(user_agent_summary, ''), action, severity, COALESCE(resource_type, ''), COALESCE(resource_id, ''),
		        COALESCE(diff, '{}'::jsonb), metadata
		   FROM audit_entries
		  WHERE `+strings.Join(where, " AND ")+`
		  ORDER BY ts DESC
		  LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("list audit entries: %w", err)
	}
	defer rows.Close()
	out := []domain.AuditEntry{}
	for rows.Next() {
		var (
			e          domain.AuditEntry
			id         uuid.UUID
			actor, pat *uuid.UUID
			severity   string
			diffJSON   []byte
			metaJSON   []byte
		)
		if err := rows.Scan(&id, &e.At, &actor, &e.ActorUsername, &pat, &e.IPAddress, &e.UserAgent, &e.Action, &severity,
			&e.ResourceType, &e.ResourceID, &diffJSON, &metaJSON); err != nil {
			return nil, fmt.Errorf("scan audit entry: %w", err)
		}
		e.ID, e.Severity = domain.AuditEntryID(id), domain.AuditSeverity(severity)
		if actor != nil {
			u := domain.UserID(*actor)
			e.ActorUserID = &u
		}
		if pat != nil {
			p := domain.PATID(*pat)
			e.ActorPATID = &p
		}
		var diff map[string]map[string]string
		_ = json.Unmarshal(diffJSON, &diff)
		for field, d := range diff {
			e.Diff = append(e.Diff, domain.FieldChange{Field: field, Before: d["before"], After: d["after"]})
		}
		_ = json.Unmarshal(metaJSON, &e.Metadata)
		out = append(out, e)
	}
	return out, rows.Err()
}

// auditWhere renders the filter as a WHERE clause with its bind arguments.
func auditWhere(f domain.AuditFilter) ([]string, []any) {
	where := []string{"true"}
	args := []any{}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if !f.From.IsZero() {
		add("ts >= $%d", f.From)
	}
	if !f.To.IsZero() {
		add("ts < $%d", f.To)
	}
	if f.ActorUserID != nil {
		add("actor_user_id = $%d", f.ActorUserID.UUID())
	}
	if f.ResourceType != "" {
		add("resource_type = $%d", f.ResourceType)
	}
	if f.ResourceID != "" {
		add("resource_id = $%d", f.ResourceID)
	}
	if len(f.Actions) > 0 {
		add("action = ANY($%d)", f.Actions)
	}
	if f.MinSeverity != "" {
		add("CASE severity WHEN 'high' THEN 3 WHEN 'warn' THEN 2 ELSE 1 END >= $%d", f.MinSeverity.Rank())
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		args = append(args, q)
		n := len(args)
		where = append(where, fmt.Sprintf("(action ILIKE '%%' || $%d || '%%' OR metadata::text ILIKE '%%' || $%d || '%%' OR COALESCE(actor_username, '') ILIKE '%%' || $%d || '%%')", n, n, n))
	}
	return where, args
}

func severityOrInfo(s domain.AuditSeverity) domain.AuditSeverity {
	if s == "" {
		return domain.AuditInfo
	}
	return s
}

func patIDParam(id *domain.PATID) any {
	if id == nil {
		return nil
	}
	return id.UUID()
}
