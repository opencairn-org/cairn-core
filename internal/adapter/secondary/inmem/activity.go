package inmem

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// ActivityRepo is an in-memory slice of port.ActivityRepo for tests: the
// activity/source rows plus the match bookkeeping the recluster and source
// action use cases touch. Methods it does not implement are inherited from the
// embedded interface and panic when called, surfacing a missing fake method.
type ActivityRepo struct {
	port.ActivityRepo
	mu          sync.Mutex
	Activities  map[domain.ActivityID]domain.Activity
	Sources     map[domain.SourceID]domain.ActivitySource
	Redirects   map[domain.ActivityID]domain.ActivityID
	Constraints []domain.MatchConstraint
	MatchState  map[domain.ActivityID]string
	// ReimportStatus records SetSourceReimportStatus calls per source.
	ReimportStatus map[domain.SourceID]domain.ReimportStatus
	// Following: viewer → followed users, for ListFollowingFeed.
	Following map[domain.UserID]map[domain.UserID]bool
}

// NewActivityRepo returns an empty repo.
func NewActivityRepo() *ActivityRepo {
	return &ActivityRepo{
		Activities:     map[domain.ActivityID]domain.Activity{},
		Sources:        map[domain.SourceID]domain.ActivitySource{},
		Redirects:      map[domain.ActivityID]domain.ActivityID{},
		MatchState:     map[domain.ActivityID]string{},
		ReimportStatus: map[domain.SourceID]domain.ReimportStatus{},
		Following:      map[domain.UserID]map[domain.UserID]bool{},
	}
}

func (r *ActivityRepo) GetActivity(_ context.Context, id domain.ActivityID) (domain.Activity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.Activities[id]
	if !ok {
		return domain.Activity{}, domain.ErrNotFound
	}
	return a, nil
}

func (r *ActivityRepo) SaveActivity(_ context.Context, a domain.Activity) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Activities[a.ID] = a
	return nil
}

func (r *ActivityRepo) SoftDeleteActivity(_ context.Context, id domain.ActivityID, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.Activities[id]
	a.ID = id
	t := at
	a.DeletedAt = &t
	r.Activities[id] = a
	return nil
}

func (r *ActivityRepo) ActivityTotals(_ context.Context, userID domain.UserID) (port.ActivityTotalsResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out port.ActivityTotalsResult
	for _, a := range r.Activities {
		if a.UserID == userID && a.DeletedAt == nil {
			out.Count++
		}
	}
	return out, nil
}

func (r *ActivityRepo) ActivityTotalsForRange(_ context.Context, userID domain.UserID, start, end time.Time) (port.ActivityTotalsResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out port.ActivityTotalsResult
	for _, a := range r.Activities {
		if a.UserID != userID || a.DeletedAt != nil || a.StartTime.Before(start) || !a.StartTime.Before(end) {
			continue
		}
		out.Count++
		if a.Summary.DistanceM != nil {
			out.DistanceM += *a.Summary.DistanceM
		}
		if a.Summary.ElevationGainM != nil {
			out.ElevationGainM += *a.Summary.ElevationGainM
		}
		out.MovingS += a.MovingDuration.Seconds()
	}
	return out, nil
}

func (r *ActivityRepo) ListRecentActivitiesForUser(_ context.Context, userID domain.UserID, limit int) ([]domain.Activity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Activity
	for _, a := range r.Activities {
		if a.UserID == userID && a.DeletedAt == nil {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartTime.After(out[j].StartTime) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *ActivityRepo) GetSource(_ context.Context, id domain.SourceID) (domain.ActivitySource, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.Sources[id]
	if !ok {
		return domain.ActivitySource{}, domain.ErrNotFound
	}
	return s, nil
}

func (r *ActivityRepo) SaveSource(_ context.Context, s domain.ActivitySource) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Sources[s.ID] = s
	return nil
}

func (r *ActivityRepo) ListSourcesForActivity(_ context.Context, id domain.ActivityID) ([]domain.ActivitySource, error) {
	return r.sourcesOf(id, false), nil
}

func (r *ActivityRepo) ListAllSourcesForActivity(_ context.Context, id domain.ActivityID) ([]domain.ActivitySource, error) {
	return r.sourcesOf(id, true), nil
}

func (r *ActivityRepo) sourcesOf(id domain.ActivityID, includeDetached bool) []domain.ActivitySource {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.ActivitySource
	for _, s := range r.Sources {
		if s.ActivityID != id {
			continue
		}
		if !includeDetached && s.Status == domain.SourceStatusDetached {
			continue
		}
		out = append(out, s)
	}
	return out
}

func (r *ActivityRepo) DetachSource(_ context.Context, id domain.SourceID, reason string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.Sources[id]
	if !ok {
		return domain.ErrNotFound
	}
	s.Status = domain.SourceStatusDetached
	s.StatusReason = reason
	s.UpdatedAt = at
	r.Sources[id] = s
	return nil
}

func (r *ActivityRepo) ReassignSource(_ context.Context, sourceID domain.SourceID, newActivityID domain.ActivityID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.Sources[sourceID]
	s.ActivityID = newActivityID
	r.Sources[sourceID] = s
	return nil
}

func (r *ActivityRepo) SetSourceReimportStatus(_ context.Context, id domain.SourceID, status domain.ReimportStatus, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ReimportStatus[id] = status
	if s, ok := r.Sources[id]; ok {
		s.ReimportStatus = status
		s.ReimportStatusReason = reason
		r.Sources[id] = s
	}
	return nil
}

func (r *ActivityRepo) SetSourceRawBlob(_ context.Context, id domain.SourceID, blobID, contentType string, sizeBytes int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.Sources[id]
	s.RawBlobID, s.RawContentType, s.RawSizeBytes = blobID, contentType, sizeBytes
	r.Sources[id] = s
	return nil
}

func (r *ActivityRepo) SetActivityMatchState(_ context.Context, id domain.ActivityID, confidence string, _ bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.MatchState[id] = confidence
	return nil
}

func (r *ActivityRepo) AddActivityRedirect(_ context.Context, oldID, newID domain.ActivityID, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Redirects[oldID] = newID
	return nil
}

func (r *ActivityRepo) ListMatchConstraints(_ context.Context, userID domain.UserID) ([]domain.MatchConstraint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.MatchConstraint
	for _, c := range r.Constraints {
		if c.UserID == userID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (r *ActivityRepo) AddMatchConstraint(_ context.Context, c domain.MatchConstraint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c.ID == (domain.MatchConstraintID{}) {
		c.ID = domain.MatchConstraintID(uuid.New())
	}
	r.Constraints = append(r.Constraints, c)
	return nil
}

func (r *ActivityRepo) ListSourceRecordsInBucket(_ context.Context, userID domain.UserID, from, to time.Time) ([]domain.SourceMatchRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.SourceMatchRecord
	for _, s := range r.Sources {
		if s.UserID != userID || s.Status == domain.SourceStatusDetached {
			continue
		}
		st := s.Parsed.StartTime
		if st.Before(from) || !st.Before(to) {
			continue
		}
		out = append(out, domain.SourceMatchRecord{
			SourceID:          s.ID,
			ActivityID:        s.ActivityID,
			UserID:            s.UserID,
			Provider:          s.Provider,
			SourceWorkerName:  s.SourceWorkerName,
			ExternalAccountID: s.ExternalAccountID,
			ExternalID:        s.ExternalID,
			SportClass:        string(s.Parsed.Type),
			StartUTC:          st,
			DistanceM:         s.Parsed.Summary.DistanceM,
			MovingS:           int64(s.Parsed.MovingDuration.Seconds()),
			ElapsedS:          int64(s.Parsed.ElapsedDuration.Seconds()),
			StartLat:          s.StartLat,
			StartLng:          s.StartLng,
			Status:            s.Status,
		})
	}
	return out, nil
}

// PassTx is a port.TxManager that runs the function inline.
type PassTx struct{}

func (PassTx) InTx(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }

// DefaultSettings is a port.UserSettingsRepo returning the default merge policy.
type DefaultSettings struct{ port.UserSettingsRepo }

func (DefaultSettings) GetMergePolicy(_ context.Context, _ domain.UserID, t domain.ActivityType) (domain.MergePolicy, error) {
	return domain.DefaultMergePolicyFor(t), nil
}

// SourceDenylist is an in-memory port.SourceDenylistRepo.
type SourceDenylist struct {
	mu      sync.Mutex
	Entries []domain.SourceDenylistEntry
}

func (d *SourceDenylist) Add(_ context.Context, e domain.SourceDenylistEntry) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Entries = append(d.Entries, e)
	return nil
}

func (d *SourceDenylist) IsDenied(_ context.Context, userID domain.UserID, provider string, _ *domain.ExternalAccountID, externalID string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range d.Entries {
		if e.UserID == userID && e.Provider == provider && e.ExternalID == externalID {
			return true, nil
		}
	}
	return false, nil
}

// live returns the user's non-deleted activities, newest first.
func (r *ActivityRepo) live(userID domain.UserID) []domain.Activity {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Activity
	for _, a := range r.Activities {
		if a.UserID == userID && a.DeletedAt == nil {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartTime.After(out[j].StartTime) })
	return out
}

// matches applies the filter dimensions the fake supports: type, discipline,
// time range and gear.
func matches(a domain.Activity, f port.ActivityListFilter, ignoreType, ignoreDiscipline bool) bool {
	if !ignoreType && f.Type != "" && string(a.Type) != f.Type {
		return false
	}
	if !ignoreDiscipline && f.Discipline != "" && string(a.Discipline) != f.Discipline {
		return false
	}
	if !f.From.IsZero() && a.StartTime.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && !a.StartTime.Before(f.To) {
		return false
	}
	return matchesGear(a, f.GearIDs)
}

func matchesGear(a domain.Activity, ids []domain.GearID) bool {
	if len(ids) == 0 {
		return true
	}
	if a.GearID == nil {
		return false
	}
	for _, g := range ids {
		if g == *a.GearID {
			return true
		}
	}
	return false
}

func (r *ActivityRepo) ActivityFacets(_ context.Context, userID domain.UserID, f port.ActivityListFilter) ([]port.ActivityFacet, []port.ActivityFacet, int, error) {
	all := r.live(userID)
	types, discs := map[string]int{}, map[string]int{}
	for _, a := range all {
		if matches(a, f, true, false) {
			types[string(a.Type)]++
		}
		if matches(a, f, false, true) && a.Discipline != "" {
			discs[string(a.Discipline)]++
		}
	}
	return facetList(types), facetList(discs), len(all), nil
}

func facetList(m map[string]int) []port.ActivityFacet {
	out := make([]port.ActivityFacet, 0, len(m))
	for v, c := range m {
		out = append(out, port.ActivityFacet{Value: v, Count: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	return out
}

func (r *ActivityRepo) ListActivitiesFiltered(_ context.Context, userID domain.UserID, f port.ActivityListFilter) ([]domain.Activity, int, error) {
	var hits []domain.Activity
	for _, a := range r.live(userID) {
		if matches(a, f, false, false) {
			hits = append(hits, a)
		}
	}
	if f.Sort == "date_asc" {
		sort.Slice(hits, func(i, j int) bool { return hits[i].StartTime.Before(hits[j].StartTime) })
	}
	matched := len(hits)
	if f.Offset > 0 {
		if f.Offset >= len(hits) {
			hits = nil
		} else {
			hits = hits[f.Offset:]
		}
	}
	if f.Limit > 0 && len(hits) > f.Limit {
		hits = hits[:f.Limit]
	}
	return hits, matched, nil
}

func (r *ActivityRepo) ActivityYearStats(_ context.Context, userID domain.UserID) ([]port.ActivityYearStat, error) {
	years := map[int]*port.ActivityYearStat{}
	for _, a := range r.live(userID) {
		y := a.StartTime.UTC().Year()
		if years[y] == nil {
			years[y] = &port.ActivityYearStat{Year: y}
		}
		years[y].Count++
	}
	out := make([]port.ActivityYearStat, 0, len(years))
	for _, y := range years {
		out = append(out, *y)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Year > out[j].Year })
	return out, nil
}

// Following lists who each user follows, for ListFollowingFeed.
func (r *ActivityRepo) ListFollowingFeed(_ context.Context, viewerID domain.UserID, limit, offset int) ([]domain.Activity, error) {
	r.mu.Lock()
	followed := r.Following[viewerID]
	var out []domain.Activity
	for _, a := range r.Activities {
		if a.DeletedAt != nil || a.UserID == viewerID || a.Privacy == domain.PrivacyPrivate {
			continue
		}
		if followed[a.UserID] {
			out = append(out, a)
		}
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].StartTime.After(out[j].StartTime) })
	if offset > 0 {
		if offset >= len(out) {
			return nil, nil
		}
		out = out[offset:]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListActivitiesNeedingReview: the fake tracks no review flag, so nothing is pending.
func (r *ActivityRepo) ListActivitiesNeedingReview(context.Context, domain.UserID) ([]domain.ActivityReviewItem, error) {
	return nil, nil
}

// UserFootprints counts the fake's activities per user.
func (r *ActivityRepo) UserFootprints(_ context.Context, userIDs []domain.UserID) (map[domain.UserID]port.UserFootprint, error) {
	out := map[domain.UserID]port.UserFootprint{}
	for _, uid := range userIDs {
		fp := port.UserFootprint{}
		for _, a := range r.Activities {
			if a.UserID == uid && !a.IsDeleted() {
				fp.Activities++
			}
		}
		out[uid] = fp
	}
	return out, nil
}

// SourceWorkerGroups groups the fake's active sources by worker name.
func (r *ActivityRepo) SourceWorkerGroups(context.Context) ([]port.SourceWorkerGroup, error) {
	by := map[string]*port.SourceWorkerGroup{}
	for _, s := range r.Sources {
		if s.Status != domain.SourceStatusActive {
			continue
		}
		g, ok := by[s.SourceWorkerName]
		if !ok {
			g = &port.SourceWorkerGroup{WorkerName: s.SourceWorkerName}
			by[s.SourceWorkerName] = g
		}
		g.Sources++
	}
	out := []port.SourceWorkerGroup{}
	for _, g := range by {
		out = append(out, *g)
	}
	return out, nil
}
