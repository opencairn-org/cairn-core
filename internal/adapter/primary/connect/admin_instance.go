package connect

import (
	"context"

	connectrpc "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/opencairn-org/cairn-core/gen/proto/cairn/v1"
	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/usecase/instance"
)

// Instance settings, outbound-email check and system health.

func (s *AdminServer) GetInstanceSettings(ctx context.Context, _ *v1.GetInstanceSettingsRequest) (*v1.GetInstanceSettingsResponse, error) {
	if s.Settings == nil {
		return nil, unavailable("instance settings")
	}
	cur, err := s.Settings.Get(ctx)
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.GetInstanceSettingsResponse{Settings: instanceSettingsToProto(cur)}, nil
}

func (s *AdminServer) UpdateInstanceSettings(ctx context.Context, req *v1.UpdateInstanceSettingsRequest) (*v1.UpdateInstanceSettingsResponse, error) {
	if s.Settings == nil {
		return nil, unavailable("instance settings")
	}
	patch, err := instancePatchFromProto(req)
	if err != nil {
		return nil, err
	}
	cur, err := s.Settings.Update(ctx, s.actor(ctx), patch)
	if err != nil {
		return nil, actionErr(err)
	}
	return &v1.UpdateInstanceSettingsResponse{Settings: instanceSettingsToProto(cur)}, nil
}

func (s *AdminServer) SendTestEmail(ctx context.Context, req *v1.SendTestEmailRequest) (*v1.SendTestEmailResponse, error) {
	if s.TestEmail == nil {
		return nil, unavailable("outbound email")
	}
	to := req.GetToAddress()
	if to == "" {
		if u, err := s.Users.GetUser(ctx, s.actor(ctx).UserIDOrZero()); err == nil {
			to = u.Email
		}
	}
	took, err := s.TestEmail.Send(ctx, to)
	if err != nil {
		if ae := actionErr(err); connectrpc.CodeOf(ae) != connectrpc.CodeInternal {
			return nil, ae
		}
		msg := err.Error()
		return &v1.SendTestEmailResponse{Delivered: false, ErrorMessage: &msg, Duration: durationpb.New(took)}, nil
	}
	return &v1.SendTestEmailResponse{Delivered: true, Duration: durationpb.New(took)}, nil
}

func (s *AdminServer) GetSystemHealth(ctx context.Context, _ *v1.GetSystemHealthRequest) (*v1.GetSystemHealthResponse, error) {
	if s.Health == nil {
		return nil, unavailable("system health")
	}
	r := s.Health.Check(ctx)
	return &v1.GetSystemHealthResponse{
		Overall: healthStateToProto(r.Overall), Database: componentToProto(r.Database), Queue: componentToProto(r.Queue),
		Storage: componentToProto(r.Storage), Email: componentToProto(r.Email),
		WorkersHealthy: connectClampInt32(r.Workers.Healthy), WorkersDegraded: connectClampInt32(r.Workers.Degraded), WorkersOffline: connectClampInt32(r.Workers.Offline),
		JobsQueued: connectClampInt32(r.JobsQueued), JobsRunning: connectClampInt32(r.JobsRunning), JobsDeadLetter: connectClampInt32(r.JobsDLQ),
		CheckedAt: timestamppb.New(r.CheckedAt),
	}, nil
}

// ---- converters ----

func instanceSettingsToProto(s domain.InstanceSettings) *v1.InstanceSettings {
	out := &v1.InstanceSettings{
		InstanceName: s.Name, InstanceDescription: s.Description, SupportEmail: s.SupportEmail, InstanceUrl: s.URL,
		LogoUrl: s.LogoURL, PrimaryColor: s.PrimaryColor, DefaultLocale: s.DefaultLocale, DefaultTimezone: s.DefaultTimezone,
		DefaultUnits: unitsToProto(s.DefaultUnits), RegistrationOpen: s.RegistrationOpen, RequireInvite: s.RequireInvite,
		AllowedEmailDomains: s.AllowedEmailDomains, EmailAdapter: emailAdapterToProto(s.EmailAdapter), EmailConfigured: s.EmailConfigured,
		EmailFromAddress: s.EmailFromAddress, StorageAdapter: storageAdapterToProto(s.StorageAdapter), StorageConfigured: s.StorageConfigured,
		UpdatedAt: timestamppb.New(s.UpdatedAt),
	}
	if s.PerUserStorageQuotaBytes != nil {
		v := *s.PerUserStorageQuotaBytes
		out.PerUserStorageQuotaBytes = &v
	}
	if s.PerUserMaxExternalAccounts != nil {
		v := connectClampInt32(*s.PerUserMaxExternalAccounts)
		out.PerUserMaxExternalAccounts = &v
	}
	return out
}

// instancePatchFromProto applies field_mask (empty = every editable field).
func instancePatchFromProto(req *v1.UpdateInstanceSettingsRequest) (domain.InstanceSettingsPatch, error) {
	in := req.GetSettings()
	if in == nil {
		return domain.InstanceSettingsPatch{}, connectrpc.NewError(connectrpc.CodeInvalidArgument, "settings are required")
	}
	mask, err := instanceMask(req.GetFieldMask())
	if err != nil {
		return domain.InstanceSettingsPatch{}, err
	}
	has := func(f string) bool { return len(mask) == 0 || mask[f] }
	str := func(f string, v string) *string {
		if !has(f) {
			return nil
		}
		return &v
	}
	p := domain.InstanceSettingsPatch{
		Name: str("instance_name", in.GetInstanceName()), Description: str("instance_description", in.GetInstanceDescription()),
		SupportEmail: str("support_email", in.GetSupportEmail()), URL: str("instance_url", in.GetInstanceUrl()),
		LogoURL: str("logo_url", in.GetLogoUrl()), PrimaryColor: str("primary_color", in.GetPrimaryColor()),
		DefaultLocale: str("default_locale", in.GetDefaultLocale()), DefaultTimezone: str("default_timezone", in.GetDefaultTimezone()),
	}
	if has("default_units") && in.GetDefaultUnits() != v1.UnitSystem_UNIT_SYSTEM_UNSPECIFIED {
		u := domain.UserUnits(unitsFromProto(in.GetDefaultUnits()))
		p.DefaultUnits = &u
	}
	if has("registration_open") {
		v := in.GetRegistrationOpen()
		p.RegistrationOpen = &v
	}
	if has("require_invite") {
		v := in.GetRequireInvite()
		p.RequireInvite = &v
	}
	if has("allowed_email_domains") {
		v := in.GetAllowedEmailDomains()
		p.AllowedEmailDomains = &v
	}
	if has("per_user_storage_quota_bytes") {
		v := in.PerUserStorageQuotaBytes
		p.PerUserStorageQuotaBytes = &v
	}
	if has("per_user_max_external_accounts") {
		var v *int
		if in.PerUserMaxExternalAccounts != nil {
			n := int(in.GetPerUserMaxExternalAccounts())
			v = &n
		}
		p.PerUserMaxExternalAccounts = &v
	}
	return p, nil
}

// instanceMask validates the editable field names of an update.
func instanceMask(fields []string) (map[string]bool, error) {
	mask := map[string]bool{}
	for _, f := range fields {
		switch f {
		case "instance_name", "instance_description", "support_email", "instance_url", "logo_url", "primary_color",
			"default_locale", "default_timezone", "default_units", "registration_open", "require_invite", "allowed_email_domains",
			"per_user_storage_quota_bytes", "per_user_max_external_accounts":
			mask[f] = true
		default:
			return nil, connectrpc.NewError(connectrpc.CodeInvalidArgument, "unknown or read-only field_mask entry "+f)
		}
	}
	return mask, nil
}

func emailAdapterToProto(a string) v1.EmailAdapterKind {
	switch a {
	case "none", "":
		return v1.EmailAdapterKind_EMAIL_ADAPTER_KIND_NONE
	case "smtp":
		return v1.EmailAdapterKind_EMAIL_ADAPTER_KIND_SMTP
	case "ses":
		return v1.EmailAdapterKind_EMAIL_ADAPTER_KIND_SES
	case "resend":
		return v1.EmailAdapterKind_EMAIL_ADAPTER_KIND_RESEND
	case "mailgun":
		return v1.EmailAdapterKind_EMAIL_ADAPTER_KIND_MAILGUN
	}
	return v1.EmailAdapterKind_EMAIL_ADAPTER_KIND_UNSPECIFIED
}

func storageAdapterToProto(a string) v1.StorageAdapterKind {
	switch a {
	case "minio":
		return v1.StorageAdapterKind_STORAGE_ADAPTER_KIND_MINIO
	case "aws_s3":
		return v1.StorageAdapterKind_STORAGE_ADAPTER_KIND_AWS_S3
	case "gcs":
		return v1.StorageAdapterKind_STORAGE_ADAPTER_KIND_GCS
	case "r2":
		return v1.StorageAdapterKind_STORAGE_ADAPTER_KIND_R2
	case "local_fs":
		return v1.StorageAdapterKind_STORAGE_ADAPTER_KIND_LOCAL_FS
	}
	return v1.StorageAdapterKind_STORAGE_ADAPTER_KIND_UNSPECIFIED
}

func healthStateToProto(s instance.HealthState) v1.HealthState {
	switch s {
	case instance.Healthy:
		return v1.HealthState_HEALTH_STATE_HEALTHY
	case instance.Degraded:
		return v1.HealthState_HEALTH_STATE_DEGRADED
	case instance.Unhealthy:
		return v1.HealthState_HEALTH_STATE_UNHEALTHY
	}
	return v1.HealthState_HEALTH_STATE_UNSPECIFIED
}

func componentToProto(c instance.Component) *v1.ComponentHealth {
	return &v1.ComponentHealth{State: healthStateToProto(c.State), Detail: c.Detail, Measurements: c.Measurements}
}
