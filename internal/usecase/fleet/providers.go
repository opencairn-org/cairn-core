package fleet

import (
	"context"
	"errors"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/audit"
)

// Providers lists the providers workers declared, each with the package
// bound to it.
func (f *Fleet) Providers(ctx context.Context) ([]domain.ProviderMeta, error) {
	if f.Registry == nil {
		return nil, port.NewActionError(port.ActionErrorUnavailable, "the provider registry is not available")
	}
	list, err := f.Registry.List(ctx)
	if err != nil {
		return nil, internalErr("list providers", err)
	}
	return list, nil
}

// ProviderIDs suggests provider ids for new enrollments; any valid id is
// still accepted.
func (f *Fleet) ProviderIDs(ctx context.Context) []string {
	list, err := f.Providers(ctx)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.ID)
	}
	return out
}

// UnbindProvider drops a provider's declared meta so the next manifest,
// from whichever package publishes it, binds the provider again.
func (f *Fleet) UnbindProvider(ctx context.Context, actor audit.Actor, id string) error {
	if f.Registry == nil {
		return port.NewActionError(port.ActionErrorUnavailable, "the provider registry is not available")
	}
	if !domain.ValidProviderID(id) {
		return port.NewActionError(port.ActionErrorInvalid, "invalid provider id")
	}
	if err := f.Registry.Delete(ctx, id); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return port.NewActionError(port.ActionErrorNotFound, "no such provider")
		}
		return internalErr("unbind provider", err)
	}
	f.Audit.High(ctx, actor, domain.AuditProviderUnbound, "provider", id, nil)
	return nil
}
