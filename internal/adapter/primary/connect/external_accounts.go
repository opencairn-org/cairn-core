package connect

import (
	"context"
	"fmt"
	"strconv"

	connectrpc "connectrpc.com/connect/v2"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/gen/proto/cairn/v1/cairnv1connect"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/view"
)

// ExternalAccountServer implements the backed parts of
// cairnv1connect.ExternalAccountServiceHandler: the sync-status reads and the
// account toggles the REST /api/accounts routes offer.
type ExternalAccountServer struct {
	cairnv1connect.UnimplementedExternalAccountServiceHandler

	Accounts            *view.Accounts
	Repo                port.ExternalAccountRepo
	Insight             port.SyncInsight
	DefaultPollInterval int // seconds
}

var _ cairnv1connect.ExternalAccountServiceHandler = (*ExternalAccountServer)(nil)

// NewExternalAccountServer wires the server.
func NewExternalAccountServer(accounts *view.Accounts, repo port.ExternalAccountRepo, insight port.SyncInsight, defaultPollSeconds int) *ExternalAccountServer {
	return &ExternalAccountServer{Accounts: accounts, Repo: repo, Insight: insight, DefaultPollInterval: defaultPollSeconds}
}

func (s *ExternalAccountServer) accounts() (*view.Accounts, error) {
	if s.Accounts == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "provider accounts are not available on this instance")
	}
	return s.Accounts, nil
}

// owned resolves the caller's account from a raw id.
func (s *ExternalAccountServer) owned(ctx context.Context, rawID string) (domain.ExternalAccount, *view.Accounts, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return domain.ExternalAccount{}, nil, err
	}
	accts, err := s.accounts()
	if err != nil {
		return domain.ExternalAccount{}, nil, err
	}
	id, err := uuid.Parse(rawID)
	if err != nil {
		return domain.ExternalAccount{}, nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "invalid account id")
	}
	acct, err := accts.Owned(ctx, userID, domain.ExternalAccountID(id))
	if err != nil {
		return domain.ExternalAccount{}, nil, connectrpc.NewError(connectrpc.CodeNotFound, "not found")
	}
	return acct, accts, nil
}

func (s *ExternalAccountServer) ListExternalAccounts(ctx context.Context, _ *v1.ListExternalAccountsRequest) (*v1.ListExternalAccountsResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	accts, err := s.accounts()
	if err != nil {
		return nil, err
	}
	rows, err := accts.List(ctx, userID)
	if err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("list accounts: %w", err))
	}
	out := &v1.ListExternalAccountsResponse{DefaultPollIntervalSeconds: connectClampInt32(s.DefaultPollInterval)}
	for _, row := range rows {
		out.Accounts = append(out.Accounts, accountRowToProto(row))
	}
	return out, nil
}

func accountRowToProto(row view.AccountRow) *v1.ExternalAccount {
	a := row.Account
	out := &v1.ExternalAccount{
		Id:                  a.ID.String(),
		UserId:              a.UserID.String(),
		Provider:            a.Provider,
		ProviderAccountId:   a.ProviderAccountID,
		DisplayLabel:        a.DisplayLabel,
		AssignedWorkerName:  a.AssignedWorkerName,
		Status:              accountStatusToProto(a.Status),
		StatusReason:        a.StatusReason,
		GrantedScopes:       a.GrantedScopes,
		WebhookSubscribed:   a.WebhookSubscribed,
		CreatedAt:           timestamppb.New(a.CreatedAt),
		UpdatedAt:           timestamppb.New(a.UpdatedAt),
		Imported:            connectClampInt32(row.Imported),
		ProviderTotal:       connectClampInt32(row.ProviderTotal),
		Discovering:         row.Discovering,
		RateLimited:         row.RateLimited,
		PollIntervalSeconds: connectClampInt32(row.PollInterval),
		AutoImportEnabled:   a.AutoImportEnabled,
		Queue: &v1.QueueCounts{
			Pending: connectClampInt32(row.Queue[domain.ImportStatusPending]), InProgress: connectClampInt32(row.Queue[domain.ImportStatusInProgress]),
			Done: connectClampInt32(row.Queue[domain.ImportStatusDone]), Failed: connectClampInt32(row.Queue[domain.ImportStatusFailed]),
			Skipped: connectClampInt32(row.Queue[domain.ImportStatusSkipped]),
		},
		OutOfDate: connectClampInt32(row.OutOfDate),
	}
	if a.WebhookSubscribedAt != nil {
		out.WebhookSubscribedAt = timestamppb.New(*a.WebhookSubscribedAt)
	}
	if a.WebhookSubscriptionID != "" {
		v := a.WebhookSubscriptionID
		out.WebhookSubscriptionId = &v
	}
	if a.LastSyncAt != nil {
		out.LastSyncAt = timestamppb.New(*a.LastSyncAt)
	}
	if a.LastSuccessfulSyncAt != nil {
		out.LastSuccessfulSyncAt = timestamppb.New(*a.LastSuccessfulSyncAt)
	}
	if a.SyncWatermark != nil {
		out.Watermark = timestamppb.New(*a.SyncWatermark)
	}
	if a.ConnectionID != nil {
		v := a.ConnectionID.String()
		out.ConnectionId = &v
	}
	if rl := a.RateLimit; rl != nil {
		out.RateLimit = &v1.ExternalAccountRateLimit{
			ShortRemaining: connectClampInt32(rl.ShortWindowLimit - rl.ShortWindowUsed), ShortLimit: connectClampInt32(rl.ShortWindowLimit), ShortResetAt: timestamppb.New(rl.ShortWindowResetAt),
			LongRemaining: connectClampInt32(rl.LongWindowLimit - rl.LongWindowUsed), LongLimit: connectClampInt32(rl.LongWindowLimit), LongResetAt: timestamppb.New(rl.LongWindowResetAt),
		}
	}
	if !row.Budget.Empty() {
		out.RateLimitBudget = &v1.RateLimitBudget{Short: budgetWindowToProto(row.Budget.Short), Daily: budgetWindowToProto(row.Budget.Daily)}
	}
	for _, p := range row.Pools {
		out.WorkerPools = append(out.WorkerPools, poolToProto(p))
	}
	return out
}

func budgetWindowToProto(w *domain.BudgetWindow) *v1.RateLimitWindow {
	if w == nil {
		return nil
	}
	return &v1.RateLimitWindow{Used: connectClampInt32(w.Used), Limit: connectClampInt32(w.Limit), ResetsAt: timestamppb.New(w.ResetsAt)}
}

func poolToProto(p domain.WorkerPoolInfo) *v1.WorkerPoolInfo {
	return &v1.WorkerPoolInfo{Name: p.Name, Package: p.Package, ParserVersion: p.ParserVersion, Release: p.Release, Online: p.Online, Compatible: p.Compatible}
}

func accountStatusToProto(s domain.ExternalAccountStatus) v1.ExternalAccountStatus {
	switch s {
	case domain.ExternalAccountStatusActive:
		return v1.ExternalAccountStatus_EXTERNAL_ACCOUNT_STATUS_ACTIVE
	case domain.ExternalAccountStatusAuthInvalid:
		return v1.ExternalAccountStatus_EXTERNAL_ACCOUNT_STATUS_AUTH_INVALID
	case domain.ExternalAccountStatusWorkerOffline:
		return v1.ExternalAccountStatus_EXTERNAL_ACCOUNT_STATUS_WORKER_OFFLINE
	case domain.ExternalAccountStatusRateLimited:
		return v1.ExternalAccountStatus_EXTERNAL_ACCOUNT_STATUS_RATE_LIMITED
	case domain.ExternalAccountStatusNeedsMigration:
		return v1.ExternalAccountStatus_EXTERNAL_ACCOUNT_STATUS_NEEDS_MIGRATION
	case domain.ExternalAccountStatusDisabled:
		return v1.ExternalAccountStatus_EXTERNAL_ACCOUNT_STATUS_DISABLED
	}
	return v1.ExternalAccountStatus_EXTERNAL_ACCOUNT_STATUS_UNSPECIFIED
}

func (s *ExternalAccountServer) ListAvailableProviders(context.Context, *v1.ListAvailableProvidersRequest) (*v1.ListAvailableProvidersResponse, error) {
	if s.Insight == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "providers are not available on this instance")
	}
	out := &v1.ListAvailableProvidersResponse{}
	for _, p := range s.Insight.Providers() {
		auth := v1.AvailableAuthMethod_AVAILABLE_AUTH_METHOD_BASIC
		if p.OAuth {
			auth = v1.AvailableAuthMethod_AVAILABLE_AUTH_METHOD_OAUTH2
		}
		out.Providers = append(out.Providers, &v1.AvailableProvider{ProviderId: p.ID, DisplayName: p.DisplayName, AuthMethod: auth})
	}
	return out, nil
}

func (s *ExternalAccountServer) ListConnections(ctx context.Context, _ *v1.ListConnectionsRequest) (*v1.ListConnectionsResponse, error) {
	userID, err := userIDFromHandlerCtx(ctx)
	if err != nil {
		return nil, err
	}
	accts, err := s.accounts()
	if err != nil {
		return nil, err
	}
	conns, providers, err := accts.Connections(ctx, userID)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListConnectionsResponse{}
	for _, c := range conns {
		out.Connections = append(out.Connections, &v1.Connection{
			Id: c.Connection.ID.String(), Provider: c.Connection.Provider, DisplayName: c.DisplayName, Label: c.Connection.Label,
			ClientId: c.Connection.ClientID, HasSecret: c.Connection.HasSecret, Oauth: c.OAuth,
		})
	}
	for _, p := range providers {
		info := &v1.ProviderInfo{Provider: p.Provider.ID, DisplayName: p.Provider.DisplayName, Oauth: p.Provider.OAuth}
		for _, wp := range p.Pools {
			info.WorkerPools = append(info.WorkerPools, poolToProto(wp))
		}
		out.Providers = append(out.Providers, info)
	}
	return out, nil
}

func (s *ExternalAccountServer) ListSyncHistory(ctx context.Context, req *v1.ListSyncHistoryRequest) (*v1.ListSyncHistoryResponse, error) {
	acct, accts, err := s.owned(ctx, req.GetAccountId())
	if err != nil {
		return nil, err
	}
	limit, _ := pageFrom(req.GetPage())
	rows, err := accts.History(ctx, acct, limit)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListSyncHistoryResponse{Page: &v1.PageResponse{Total: -1}}
	for _, row := range rows {
		e := row.Event
		ev := &v1.SyncEvent{Kind: e.Kind, Count: connectClampInt32(e.Count), Detail: e.Detail, ExternalId: e.ExternalID,
			OccurredAt: timestamppb.New(e.OccurredAt), ExternalUrl: row.ExternalURL, Status: row.Status}
		if row.ActivityID != nil {
			ev.ActivityId = row.ActivityID.String()
		}
		out.Events = append(out.Events, ev)
	}
	return out, nil
}

func (s *ExternalAccountServer) ListQueueItems(ctx context.Context, req *v1.ListQueueItemsRequest) (*v1.ListQueueItemsResponse, error) {
	acct, accts, err := s.owned(ctx, req.GetAccountId())
	if err != nil {
		return nil, err
	}
	statuses := view.DefaultQueueStatuses
	if len(req.GetStatuses()) > 0 {
		statuses = statuses[:0:0]
		for _, st := range req.GetStatuses() {
			statuses = append(statuses, domain.ImportItemStatus(st))
		}
	}
	limit := int(req.GetPage().GetLimit())
	rows, err := accts.QueueItems(ctx, acct, statuses, limit)
	if err != nil {
		return nil, actionErr(err)
	}
	out := &v1.ListQueueItemsResponse{Page: &v1.PageResponse{Total: -1}}
	for _, row := range rows {
		out.Items = append(out.Items, queueItemToProto(row))
	}
	return out, nil
}

func queueItemToProto(row view.QueueItemRow) *v1.QueueItem {
	it := row.Item
	out := &v1.QueueItem{
		Id: it.ID.String(), ItemType: string(it.ItemType), ExternalId: it.ExternalID, Status: string(it.Status),
		Priority: connectClampInt32(it.Priority), Attempts: connectClampInt32(it.Attempts), LastError: it.LastError,
		CreatedAt: timestamppb.New(it.CreatedAt), ExternalUrl: row.ExternalURL,
	}
	if it.ItemTime != nil {
		out.ItemTime = timestamppb.New(*it.ItemTime)
	}
	if it.StartedAt != nil {
		out.StartedAt = timestamppb.New(*it.StartedAt)
	}
	if it.CompletedAt != nil {
		out.CompletedAt = timestamppb.New(*it.CompletedAt)
	}
	return out
}

// UpdateExternalAccountConfig edits the per-connection poll interval and the
// label; the pool assignment has its own REST rules and is refused here.
func (s *ExternalAccountServer) UpdateExternalAccountConfig(ctx context.Context, req *v1.UpdateExternalAccountConfigRequest) (*v1.UpdateExternalAccountConfigResponse, error) {
	acct, accts, err := s.owned(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	if s.Repo == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "provider accounts are not available on this instance")
	}
	if req.AssignedWorkerName != nil {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "assigned_worker_name is changed through PUT /api/accounts/{id}/worker")
	}
	patch := map[string]any{}
	for k, v := range req.GetConfig() {
		if k != "poll_interval_seconds" {
			return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "only config[\"poll_interval_seconds\"] is editable")
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "poll_interval_seconds must be a non-negative integer")
		}
		patch["poll_interval_seconds"] = n
	}
	if req.DisplayLabel != nil {
		patch["display_label"] = req.GetDisplayLabel()
	}
	if len(patch) == 0 {
		return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "nothing to update")
	}
	if err := s.Repo.UpdateAccountConfig(ctx, acct.ID, patch); err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("update config: %w", err))
	}
	return &v1.UpdateExternalAccountConfigResponse{Account: s.reload(ctx, accts, acct)}, nil
}

// reload renders the account row after a change; the bare row on failure.
func (s *ExternalAccountServer) reload(ctx context.Context, accts *view.Accounts, acct domain.ExternalAccount) *v1.ExternalAccount {
	rows, err := accts.List(ctx, acct.UserID)
	if err == nil {
		for _, row := range rows {
			if row.Account.ID == acct.ID {
				return accountRowToProto(row)
			}
		}
	}
	return accountRowToProto(view.AccountRow{Account: acct})
}

func (s *ExternalAccountServer) SetExternalAccountEnabled(ctx context.Context, req *v1.SetExternalAccountEnabledRequest) (*v1.SetExternalAccountEnabledResponse, error) {
	acct, accts, err := s.owned(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	if s.Repo == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "provider accounts are not available on this instance")
	}
	if err := s.Repo.SetAutoImport(ctx, acct.ID, req.GetEnabled()); err != nil {
		return nil, connectError(connectrpc.CodeInternal, fmt.Errorf("set auto import: %w", err))
	}
	acct.AutoImportEnabled = req.GetEnabled()
	return &v1.SetExternalAccountEnabledResponse{Account: s.reload(ctx, accts, acct)}, nil
}

func (s *ExternalAccountServer) TriggerSync(ctx context.Context, req *v1.TriggerSyncRequest) (*v1.TriggerSyncResponse, error) {
	acct, _, err := s.owned(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	if s.Insight == nil {
		return nil, connectrpc.NewError(connectrpc.CodeUnavailable, "sync is not available on this instance")
	}
	s.Insight.StartFullSync(acct, false)
	return &v1.TriggerSyncResponse{JobId: "discovery:" + acct.ID.String()}, nil
}
