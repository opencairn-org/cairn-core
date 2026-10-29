package domain

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

// InstanceSettings is the single-row instance configuration an admin edits
// at runtime; the email and storage fields mirror the operator's env.
type InstanceSettings struct {
	Name                string
	Description         string
	SupportEmail        string
	URL                 string
	LogoURL             string
	PrimaryColor        string
	DefaultLocale       string
	DefaultTimezone     string
	DefaultUnits        UserUnits
	RegistrationOpen    bool
	RequireInvite       bool
	AllowedEmailDomains []string
	// Read-only mirrors of the environment.
	EmailAdapter      string
	EmailConfigured   bool
	EmailFromAddress  string
	StorageAdapter    string
	StorageConfigured bool
	// Per-user limits; nil = unlimited.
	PerUserStorageQuotaBytes   *int64
	PerUserMaxExternalAccounts *int
	UpdatedAt                  time.Time
}

// InstanceSettingsPatch names the editable fields; nil leaves a field.
type InstanceSettingsPatch struct {
	Name, Description, SupportEmail, URL, LogoURL, PrimaryColor *string
	DefaultLocale, DefaultTimezone                              *string
	DefaultUnits                                                *UserUnits
	RegistrationOpen, RequireInvite                             *bool
	AllowedEmailDomains                                         *[]string
	PerUserStorageQuotaBytes                                    **int64
	PerUserMaxExternalAccounts                                  **int
}

var (
	ErrInstanceNameRequired = errors.New("instance name required")
	ErrInstanceColorInvalid = errors.New("primary color must be a hex color like #3b82f6")
	ErrInstanceURLInvalid   = errors.New("instance url must be an absolute http(s) url")
	hexColor                = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

// Apply lays the patch over the settings and validates the result; it
// reports which fields changed.
func (s *InstanceSettings) Apply(p InstanceSettingsPatch) ([]FieldChange, error) {
	diff, err := s.applyStrings(p)
	if err != nil {
		return nil, err
	}
	diff = append(diff, s.applyRest(p)...)
	if s.DefaultUnits != "" && !s.DefaultUnits.Valid() {
		return nil, errors.New("default units must be metric or imperial")
	}
	return diff, nil
}

// stringField pairs a settable text field with its validation.
type stringField struct {
	name  string
	dst   *string
	src   *string
	check func(string) error
}

func (s *InstanceSettings) applyStrings(p InstanceSettingsPatch) ([]FieldChange, error) {
	fields := []stringField{
		{"instance_name", &s.Name, p.Name, requireNonEmpty(ErrInstanceNameRequired)},
		{"instance_description", &s.Description, p.Description, nil},
		{"support_email", &s.SupportEmail, p.SupportEmail, optionalEmail},
		{"instance_url", &s.URL, p.URL, requireHTTPURL},
		{"logo_url", &s.LogoURL, p.LogoURL, ValidateAvatarURL},
		{"primary_color", &s.PrimaryColor, p.PrimaryColor, requireHexColor},
		{"default_locale", &s.DefaultLocale, p.DefaultLocale, ValidateLocale},
		{"default_timezone", &s.DefaultTimezone, p.DefaultTimezone, ValidateTimezone},
	}
	var diff []FieldChange
	for _, f := range fields {
		if f.src == nil {
			continue
		}
		v := strings.TrimSpace(*f.src)
		if f.check != nil {
			if err := f.check(v); err != nil {
				return nil, err
			}
		}
		if v != *f.dst {
			diff = append(diff, FieldChange{Field: f.name, Before: *f.dst, After: v})
			*f.dst = v
		}
	}
	return diff, nil
}

func requireNonEmpty(err error) func(string) error {
	return func(v string) error {
		if v == "" {
			return err
		}
		return nil
	}
}

func optionalEmail(v string) error {
	if v == "" {
		return nil
	}
	_, err := NormaliseEmail(v)
	return err
}

func requireHTTPURL(v string) error {
	if v == "" || ValidateAvatarURL(v) != nil {
		return ErrInstanceURLInvalid
	}
	return nil
}

func requireHexColor(v string) error {
	if !hexColor.MatchString(v) {
		return ErrInstanceColorInvalid
	}
	return nil
}

func (s *InstanceSettings) applyRest(p InstanceSettingsPatch) []FieldChange {
	var diff []FieldChange
	if p.DefaultUnits != nil && *p.DefaultUnits != s.DefaultUnits {
		diff = append(diff, FieldChange{Field: "default_units", Before: string(s.DefaultUnits), After: string(*p.DefaultUnits)})
		s.DefaultUnits = *p.DefaultUnits
	}
	if p.RegistrationOpen != nil && *p.RegistrationOpen != s.RegistrationOpen {
		diff = append(diff, FieldChange{Field: "registration_open", Before: fmtBool(s.RegistrationOpen), After: fmtBool(*p.RegistrationOpen)})
		s.RegistrationOpen = *p.RegistrationOpen
	}
	if p.RequireInvite != nil && *p.RequireInvite != s.RequireInvite {
		diff = append(diff, FieldChange{Field: "require_invite", Before: fmtBool(s.RequireInvite), After: fmtBool(*p.RequireInvite)})
		s.RequireInvite = *p.RequireInvite
	}
	if p.AllowedEmailDomains != nil {
		next := normaliseDomains(*p.AllowedEmailDomains)
		if strings.Join(next, ",") != strings.Join(s.AllowedEmailDomains, ",") {
			diff = append(diff, FieldChange{Field: "allowed_email_domains", Before: strings.Join(s.AllowedEmailDomains, ","), After: strings.Join(next, ",")})
			s.AllowedEmailDomains = next
		}
	}
	if p.PerUserStorageQuotaBytes != nil {
		diff = append(diff, FieldChange{Field: "per_user_storage_quota_bytes", Before: fmtInt64Ptr(s.PerUserStorageQuotaBytes), After: fmtInt64Ptr(*p.PerUserStorageQuotaBytes)})
		s.PerUserStorageQuotaBytes = *p.PerUserStorageQuotaBytes
	}
	if p.PerUserMaxExternalAccounts != nil {
		diff = append(diff, FieldChange{Field: "per_user_max_external_accounts", Before: fmtInt(s.PerUserMaxExternalAccounts), After: fmtInt(*p.PerUserMaxExternalAccounts)})
		s.PerUserMaxExternalAccounts = *p.PerUserMaxExternalAccounts
	}
	return diff
}

func normaliseDomains(in []string) []string {
	out := make([]string, 0, len(in))
	for _, d := range in {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			out = append(out, d)
		}
	}
	return out
}

func fmtBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func fmtInt64Ptr(v *int64) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(strings.Replace(fmtF64(ptrF(*v)), ".0", "", 1))
}

func ptrF(v int64) *float64 { f := float64(v); return &f }
