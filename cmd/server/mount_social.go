package main

import (
	"log/slog"
	"net/http"

	"github.com/opencairn-org/cairn-core/internal/adapter/primary/rest"
	"github.com/opencairn-org/cairn-core/internal/port"
	"github.com/opencairn-org/cairn-core/internal/usecase/moderation"
	"github.com/opencairn-org/cairn-core/internal/usecase/social"
)

// mountRestSocial mounts the follow graph, feeds, profiles, engagement,
// moderation, quota and club endpoints.
func mountRestSocial(mux *http.ServeMux, app *App, logger *slog.Logger) {
	auth := restAuth(app)
	graph := &social.Graph{}
	if app.Follows != nil {
		graph.Follows = app.Follows
	}
	if app.Blocks != nil {
		graph.Blocks = app.Blocks
	}
	if app.Follows != nil {
		(&rest.FollowHandlers{Auth: auth, Graph: graph, Profiles: app.ProfileViews, Feeds: app.Feeds, Log: logger}).Mount(mux)
		logger.Info("social (follow-graph) endpoints mounted")
	}
	(&rest.HomeFeedHandlers{Auth: auth, Feeds: app.Feeds}).Mount(mux)
	logger.Info("home feed endpoint mounted", "path", "/api/feed/home")
	mountRestProfiles(mux, app, auth, logger)
	mountRestShare(mux, app, logger)
	if app.Engagement != nil {
		eng := &social.Engagement{Activities: app.Activities, Repo: app.Engagement, Gate: app.Details}
		(&rest.EngagementHandlers{Auth: auth, Engagement: eng, Details: app.Details}).Mount(mux)
		logger.Info("engagement (kudos+comments) endpoints mounted")
	}
	mountRestModeration(mux, app, auth, graph, logger)
	if app.Quotas != nil {
		(&rest.QuotaHandlers{Auth: auth, Quota: app.QuotaViews, UserAdmin: app.UserAdmin}).Mount(mux)
		logger.Info("quota endpoints mounted")
	}
	if app.Clubs != nil {
		(&rest.ClubHandlers{Auth: auth, Clubs: &social.Clubs{Repo: app.Clubs}, Views: app.ClubViews, Log: logger}).Mount(mux)
		logger.Info("club endpoints mounted")
	}
}

func mountRestProfiles(mux *http.ServeMux, app *App, auth *rest.Auth, logger *slog.Logger) {
	if app.Users == nil {
		return
	}
	privacy := &social.Privacy{Users: app.Users}
	var visibility port.VisibilityRepo
	if app.Visibility != nil {
		visibility = app.Visibility
	}
	privacy.Visibility = visibility
	h := &rest.ProfileHandlers{Auth: auth, Profiles: app.ProfileViews, Privacy: privacy}
	h.Mount(mux)
	if visibility != nil {
		h.MountVisibility(mux)
	}
	logger.Info("profile endpoints mounted")
}

func mountRestModeration(mux *http.ServeMux, app *App, auth *rest.Auth, graph *social.Graph, logger *slog.Logger) {
	if app.Blocks == nil {
		return
	}
	reports := app.ModReports
	if reports == nil && app.Reports != nil {
		reports = &moderation.Reports{Reports: app.Reports}
	}
	h := &rest.ModerationHandlers{Auth: auth, Graph: graph, Profiles: app.ProfileViews, Reports: reports, UserAdmin: app.UserAdmin}
	h.Mount(mux)
	if app.Reports != nil {
		h.MountReports(mux)
		if app.ModReports != nil {
			h.MountAdmin(mux)
			if app.Moderation != nil {
				h.MountHide(mux)
			}
		}
	}
	logger.Info("moderation (block + report) endpoints mounted")
}
