package match

import "sort"

// Clustering: score all pairs in a bucket, group via constrained union-find.
// must-link unioned first; cannot-link and the ≤1-per-provider rule block
// unions; scored edges applied best-first. Deterministic (ID-ordered).

// Record is one bucket source record: stable ID + scoring features.
type Record struct {
	ID string
	// Pool is the worker pool (name) that imported the source. A cluster holds
	// at most one source per pool: two pools of one provider (docs/adr/0006)
	// may both feed an activity, two imports by one pool may not. Empty
	// means the provider alone is the identity.
	Pool string
	SourceFeatures
}

// identity is the one-per-cluster key of a record.
func (r Record) identity() string {
	if r.Pool == "" {
		return r.Provider
	}
	return r.Provider + "/" + r.Pool
}

// Band is a cluster's confidence: high→auto, medium→flag, low→keep separate.
type Band string

const (
	BandHigh   Band = "high"
	BandMedium Band = "medium"
	BandLow    Band = "low"
)

// Cluster is one group of records the matcher believes are the same activity.
type Cluster struct {
	RecordIDs  []string // sorted ascending
	Confidence Band
}

// Conflict: two same-provider records scored as a match but were refused by the
// ≤1-per-provider rule.
type Conflict struct {
	AID, BID string
	Provider string
	Score    float64
}

// Constraints carry the user's manual matching decisions as hard inputs.
type Constraints struct {
	MustLink   [][2]string // pairs forced into the same cluster
	CannotLink [][2]string // pairs forbidden from sharing a cluster
}

// Options tunes the clustering. A cluster's band is its MIN linking-edge score.
type Options struct {
	Weights       Weights
	EdgeThreshold float64 // min pairwise score to link
	HighBand      float64 // >= → high
	MediumBand    float64 // >= → medium, else low
}

// DefaultOptions are the starting thresholds; calibrate against the corpus.
func DefaultOptions() Options {
	return Options{
		Weights:       DefaultWeights(),
		EdgeThreshold: 0.55,
		HighBand:      0.82,
		MediumBand:    0.65,
	}
}

// Result is the clustering output for one bucket.
type Result struct {
	Clusters  []Cluster
	Conflicts []Conflict
}

// ClusterBucket groups the bucket's records.
func ClusterBucket(records []Record, c Constraints, opt Options) Result {
	if opt.EdgeThreshold == 0 && opt.HighBand == 0 {
		opt = DefaultOptions()
	}

	recs := append([]Record(nil), records...)
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	idx := make(map[string]int, len(recs))
	for i, r := range recs {
		idx[r.ID] = i
	}

	uf := newUnionFind(recs)
	forbidden := clusterForbiddenPairs(c.CannotLink, idx)

	// must-link first — authoritative; may override the ≤1-per-provider rule.
	for _, p := range c.MustLink {
		ai, aok := idx[p[0]]
		bi, bok := idx[p[1]]
		if aok && bok {
			uf.union(ai, bi, true)
		}
	}

	edges := clusterScoredEdges(recs, opt)
	minEdge, conflicts := clusterApplyEdges(uf, recs, edges, forbidden)
	out := clusterBuild(uf, recs, minEdge, opt)

	sort.Slice(conflicts, func(i, j int) bool {
		if conflicts[i].AID != conflicts[j].AID {
			return conflicts[i].AID < conflicts[j].AID
		}
		return conflicts[i].BID < conflicts[j].BID
	})

	return Result{Clusters: out, Conflicts: conflicts}
}

// clusterForbiddenPairs returns cannot-link pairs (canonical order) whose records are both in the bucket.
func clusterForbiddenPairs(cannot [][2]string, idx map[string]int) map[[2]string]struct{} {
	forbidden := map[[2]string]struct{}{}
	for _, p := range cannot {
		_, ok := idx[p[0]]
		_, ok2 := idx[p[1]]
		if !ok || !ok2 {
			continue
		}
		a, b := p[0], p[1]
		if a > b {
			a, b = b, a
		}
		forbidden[[2]string{a, b}] = struct{}{}
	}
	return forbidden
}

type clusterEdge struct {
	a, b  int
	score float64
}

// clusterScoredEdges returns above-threshold, ungated pairs, best-first.
func clusterScoredEdges(recs []Record, opt Options) []clusterEdge {
	var edges []clusterEdge
	for i := 0; i < len(recs); i++ {
		for j := i + 1; j < len(recs); j++ {
			s, gated := Score(recs[i].SourceFeatures, recs[j].SourceFeatures, opt.Weights)
			if gated || s < opt.EdgeThreshold {
				continue
			}
			edges = append(edges, clusterEdge{i, j, s})
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].score != edges[j].score {
			return edges[i].score > edges[j].score
		}
		if edges[i].a != edges[j].a {
			return edges[i].a < edges[j].a
		}
		return edges[i].b < edges[j].b
	})
	return edges
}

// clusterApplyEdges unions along edges; minEdge is the smallest linking-edge score per root.
func clusterApplyEdges(uf *unionFind, recs []Record, edges []clusterEdge, forbidden map[[2]string]struct{}) (map[int]float64, []Conflict) {
	minEdge := map[int]float64{}
	var conflicts []Conflict

	for _, e := range edges {
		ra, rb := uf.find(e.a), uf.find(e.b)
		if ra == rb {
			recordMinEdge(minEdge, ra, e.score)
			continue
		}
		if uf.violatesForbidden(ra, rb, forbidden) {
			continue
		}
		if uf.shareProvider(ra, rb) {
			conflicts = append(conflicts, Conflict{
				AID: recs[e.a].ID, BID: recs[e.b].ID, Provider: recs[e.a].Provider, Score: e.score,
			})
			continue
		}
		newRoot := uf.union(e.a, e.b, false)
		m := e.score
		if v, ok := minEdge[ra]; ok && v < m {
			m = v
		}
		if v, ok := minEdge[rb]; ok && v < m {
			m = v
		}
		delete(minEdge, ra)
		delete(minEdge, rb)
		minEdge[newRoot] = m
	}
	return minEdge, conflicts
}

// clusterBuild builds sorted clusters from components.
func clusterBuild(uf *unionFind, recs []Record, minEdge map[int]float64, opt Options) []Cluster {
	members := map[int][]string{}
	for i, r := range recs {
		root := uf.find(i)
		members[root] = append(members[root], r.ID)
	}

	out := make([]Cluster, 0, len(members))
	for root, ids := range members {
		sort.Strings(ids)
		band := BandHigh // singletons trivially high
		if len(ids) > 1 {
			band = bandFor(minEdge[root], opt)
		}
		out = append(out, Cluster{RecordIDs: ids, Confidence: band})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RecordIDs[0] < out[j].RecordIDs[0] })
	return out
}

func recordMinEdge(m map[int]float64, root int, score float64) {
	if v, ok := m[root]; !ok || score < v {
		m[root] = score
	}
}

func bandFor(minScore float64, opt Options) Band {
	switch {
	case minScore >= opt.HighBand:
		return BandHigh
	case minScore >= opt.MediumBand:
		return BandMedium
	default:
		return BandLow
	}
}

// union-find with per-component provider sets.
type unionFind struct {
	parent    []int
	rank      []int
	providers []map[string]int // per-root: provider → member count
	recIDs    [][]string       // per-root: member ids
}

func newUnionFind(recs []Record) *unionFind {
	uf := &unionFind{
		parent:    make([]int, len(recs)),
		rank:      make([]int, len(recs)),
		providers: make([]map[string]int, len(recs)),
		recIDs:    make([][]string, len(recs)),
	}
	for i, r := range recs {
		uf.parent[i] = i
		uf.providers[i] = map[string]int{r.identity(): 1}
		uf.recIDs[i] = []string{r.ID}
	}
	return uf
}

func (uf *unionFind) find(i int) int {
	for uf.parent[i] != i {
		uf.parent[i] = uf.parent[uf.parent[i]]
		i = uf.parent[i]
	}
	return i
}

// shareProvider reports whether two components share a provider pool (≤1 per pool).
func (uf *unionFind) shareProvider(ra, rb int) bool {
	pa, pb := uf.providers[ra], uf.providers[rb]
	if len(pb) < len(pa) {
		pa, pb = pb, pa
	}
	for p := range pa {
		if _, ok := pb[p]; ok {
			return true
		}
	}
	return false
}

// violatesForbidden reports whether merging two components co-locates a
// cannot-linked pair.
func (uf *unionFind) violatesForbidden(ra, rb int, forbidden map[[2]string]struct{}) bool {
	if len(forbidden) == 0 {
		return false
	}
	for _, a := range uf.recIDs[ra] {
		for _, b := range uf.recIDs[rb] {
			x, y := a, b
			if x > y {
				x, y = y, x
			}
			if _, bad := forbidden[[2]string{x, y}]; bad {
				return true
			}
		}
	}
	return false
}

// union merges the components of i and j and returns the new root. Guard checks
// are the caller's responsibility.
func (uf *unionFind) union(i, j int, _ bool) int {
	ri, rj := uf.find(i), uf.find(j)
	if ri == rj {
		return ri
	}
	if uf.rank[ri] < uf.rank[rj] {
		ri, rj = rj, ri
	}
	uf.parent[rj] = ri
	if uf.rank[ri] == uf.rank[rj] {
		uf.rank[ri]++
	}
	for p, n := range uf.providers[rj] {
		uf.providers[ri][p] += n
	}
	uf.recIDs[ri] = append(uf.recIDs[ri], uf.recIDs[rj]...)
	uf.providers[rj] = nil
	uf.recIDs[rj] = nil
	return ri
}
