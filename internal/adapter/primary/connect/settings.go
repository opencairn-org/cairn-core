package connect

import (
	"context"
	"fmt"
	"time"

	connectrpc "connectrpc.com/connect/v2"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/gen/proto/cairn/v1/cairnv1connect"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// SettingsServer implements cairnv1connect.SettingsServiceHandler over the
// settings core persists today: the per-activity-type merge policies
// (GET/PUT /api/settings/merge-policy). The other UserSettings sections are
// not stored anywhere yet; UpdateSettings refuses them and GetSettings leaves
// them empty. Physiology values live in MetricsService.ListAthleteMetrics.
type SettingsServer struct {
	cairnv1connect.UnimplementedSettingsServiceHandler

	Settings port.UserSettingsRepo
}

var _ cairnv1connect.SettingsServiceHandler = (*SettingsServer)(nil)

// NewSettingsServer wires the server.
func NewSettingsServer(settings port.UserSettingsRepo) *SettingsServer {
	return &SettingsServer{Settings: settings}
}

// mergePolicyField is the only field_mask entry UpdateSettings accepts today.
const mergePolicyField = "merge_policy_by_activity_type"

func (s *SettingsServer) settings() (port.UserSettingsRepo, error) {
	if s.Settings == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "settings are not available on this instance")
	}
	return s.Settings, nil
}

func (s *SettingsServer) GetSettings(ctx context.Context, _ *v1.GetSettingsRequest) (*v1.GetSettingsResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	settings, err := s.current(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &v1.GetSettingsResponse{Settings: settings}, nil
}

func (s *SettingsServer) current(ctx context.Context, userID domain.UserID) (*v1.UserSettings, error) {
	repo, err := s.settings()
	if err != nil {
		return nil, err
	}
	policies, err := repo.GetAllMergePolicies(ctx, userID)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("merge policies: %w", err))
	}
	return &v1.UserSettings{UserId: userID.String(), MergePolicyByActivityType: mergePoliciesToProto(policies)}, nil
}

// UpdateSettings replaces the merge policies when the field mask names them;
// any other field is refused rather than silently dropped.
func (s *SettingsServer) UpdateSettings(ctx context.Context, req *v1.UpdateSettingsRequest) (*v1.UpdateSettingsResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	repo, err := s.settings()
	if err != nil {
		return nil, err
	}
	if len(req.GetFieldMask()) == 0 {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "field_mask must list at least one field to update")
	}
	for _, f := range req.GetFieldMask() {
		if f != mergePolicyField {
			return nil, connectError(connectrpc.CodeInvalidArgument, fmt.Errorf("field %q is not editable on this instance", f))
		}
	}
	policies, err := mergePoliciesFromProto(req.GetSettings().GetMergePolicyByActivityType())
	if err != nil {
		return nil, err
	}
	if err := repo.SetMergePolicies(ctx, userID, policies); err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("save merge policies: %w", err))
	}
	settings, err := s.current(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &v1.UpdateSettingsResponse{Settings: settings}, nil
}

// ResetSettingsSection clears the merge policies (back to the instance
// defaults); other sections have nothing stored to reset.
func (s *SettingsServer) ResetSettingsSection(ctx context.Context, req *v1.ResetSettingsSectionRequest) (*v1.ResetSettingsSectionResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	repo, err := s.settings()
	if err != nil {
		return nil, err
	}
	if req.GetSection() != v1.SettingsSection_SETTINGS_SECTION_MERGE_POLICIES {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "only SETTINGS_SECTION_MERGE_POLICIES is stored on this instance")
	}
	if err := repo.SetMergePolicies(ctx, userID, map[domain.ActivityType]domain.MergePolicy{}); err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("reset merge policies: %w", err))
	}
	settings, err := s.current(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &v1.ResetSettingsSectionResponse{Settings: settings}, nil
}

func mergePoliciesToProto(m map[domain.ActivityType]domain.MergePolicy) map[string]*v1.ActivityTypeMergePolicy {
	out := make(map[string]*v1.ActivityTypeMergePolicy, len(m))
	for t, p := range m {
		wire := &v1.ActivityTypeMergePolicy{DefaultProviders: p.DefaultPriority}
		if len(p.Overrides) > 0 {
			wire.ByFieldGroup = make(map[string]*v1.ProviderPriorityList, len(p.Overrides))
			for g, providers := range p.Overrides {
				wire.ByFieldGroup[string(g)] = &v1.ProviderPriorityList{Providers: providers}
			}
		}
		out[string(t)] = wire
	}
	return out
}

func mergePoliciesFromProto(m map[string]*v1.ActivityTypeMergePolicy) (map[domain.ActivityType]domain.MergePolicy, error) {
	out := make(map[domain.ActivityType]domain.MergePolicy, len(m))
	for t, wire := range m {
		p := domain.MergePolicy{DefaultPriority: wire.GetDefaultProviders()}
		if len(wire.GetByFieldGroup()) > 0 {
			p.Overrides = make(map[domain.FieldGroup][]string, len(wire.GetByFieldGroup()))
			for g, list := range wire.GetByFieldGroup() {
				fg := domain.FieldGroup(g)
				if !validFieldGroup(fg) {
					return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "unknown field group: "+g)
				}
				p.Overrides[fg] = list.GetProviders()
			}
		}
		out[domain.ActivityType(t)] = p
	}
	return out, nil
}

func validFieldGroup(fg domain.FieldGroup) bool {
	for _, g := range domain.AllFieldGroups {
		if g == fg {
			return true
		}
	}
	return false
}

// secondsDuration converts float seconds to a Duration.
func secondsDuration(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
