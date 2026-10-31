package federation

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// openAudience shows everything to anyone; closedAudience shows nothing.
type openAudience struct{}

func (openAudience) For(context.Context, *domain.Activity, *domain.UserID, bool) domain.CategorySet {
	return allCats
}

type closedAudience struct{}

func (closedAudience) For(context.Context, *domain.Activity, *domain.UserID, bool) domain.CategorySet {
	return domain.CategorySet{}
}

type fakeFollows struct {
	port.FederationFollowRepo
	upserts   []domain.FederationFollow
	deleted   []domain.FederationFollowDirection
	following bool
	inboxes   []string
}

func (f *fakeFollows) Upsert(_ context.Context, ff domain.FederationFollow) error {
	f.upserts = append(f.upserts, ff)
	return nil
}

func (f *fakeFollows) Exists(context.Context, domain.UserID, string, domain.FederationFollowDirection) (bool, error) {
	return f.following, nil
}

func (f *fakeFollows) Delete(_ context.Context, _ domain.UserID, _ string, dir domain.FederationFollowDirection) error {
	f.deleted = append(f.deleted, dir)
	return nil
}

func (f *fakeFollows) ListInboundFollowerInboxes(context.Context, domain.UserID) ([]string, error) {
	return f.inboxes, nil
}

type fakeFeed struct {
	port.FederationFeedRepo
	items []domain.FederatedFeedItem
}

func (f *fakeFeed) Insert(_ context.Context, it domain.FederatedFeedItem) error {
	f.items = append(f.items, it)
	return nil
}

type fakeEngagement struct {
	port.RemoteEngagementRepo
	kudos, comments int
}

func (f *fakeEngagement) AddRemoteKudos(context.Context, domain.ActivityID, string, string) error {
	f.kudos++
	return nil
}

func (f *fakeEngagement) AddRemoteComment(context.Context, domain.ActivityID, string, string, string) error {
	f.comments++
	return nil
}

type fakeActivities struct {
	port.FederationActivityRepo
	act domain.Activity
}

func (f fakeActivities) GetActivity(context.Context, domain.ActivityID) (domain.Activity, error) {
	return f.act, nil
}

type fakeDedup struct{ seen map[string]bool }

func (d fakeDedup) SeenOrMark(_ context.Context, id string) (bool, error) {
	was := d.seen[id]
	d.seen[id] = true
	return was, nil
}

// fakeClient records deliveries and answers with status.
type fakeClient struct {
	port.FederationClient
	status    int
	delivered []string
}

func (c *fakeClient) Deliver(_ context.Context, _ *rsa.PrivateKey, keyID, inbox string, _ []byte) (int, error) {
	c.delivered = append(c.delivered, keyID+" "+inbox)
	return c.status, nil
}

type fakeKeys struct{ port.FederationKeyRepo }

func (fakeKeys) GetPrivateKey(context.Context, domain.UserID) (*rsa.PrivateKey, error) {
	return &rsa.PrivateKey{}, nil
}

type fakeBlocks struct {
	port.FederationBlockRepo
	host string
}

func (b fakeBlocks) IsBlocked(_ context.Context, h string) (bool, error) { return h == b.host, nil }

var testInstance = NewInstance("https://cairn.example/")

func inboxFixture() (*Inbox, *fakeFollows, *fakeFeed, *fakeEngagement, *fakeClient, domain.User) {
	target := domain.User{ID: domain.UserID(uuid.New()), Username: "alice"}
	follows, feed, eng, client := &fakeFollows{}, &fakeFeed{}, &fakeEngagement{}, &fakeClient{status: 202}
	in := &Inbox{
		Instance: testInstance, Dedup: fakeDedup{seen: map[string]bool{}},
		Follows: follows, Feed: feed, Engagement: eng,
		Activities: fakeActivities{act: domain.Activity{UserID: target.ID}},
		Audience:   openAudience{},
		Sender:     &Sender{Keys: fakeKeys{}, Client: client},
		Go:         func(_ string, fn func()) { fn() },
	}
	return in, follows, feed, eng, client, target
}

func deliveryOf(target domain.User, act map[string]any) Delivery {
	body, _ := json.Marshal(act)
	return Delivery{Target: target, Body: body, RemoteActorID: "https://remote.example/users/bob", RemoteInbox: "https://remote.example/inbox"}
}

func TestInbox_FollowAutoAccepts(t *testing.T) {
	in, follows, _, _, client, target := inboxFixture()
	ctx := context.Background()

	err := in.Receive(ctx, deliveryOf(target, map[string]any{"id": "f1", "type": "Follow", "object": "https://cairn.example/users/someone"}))
	if !errors.Is(err, ErrWrongTarget) {
		t.Fatalf("wrong target = %v", err)
	}
	if err := in.Receive(ctx, deliveryOf(target, map[string]any{"id": "f2", "type": "Follow", "object": "https://cairn.example/users/alice"})); err != nil {
		t.Fatal(err)
	}
	if len(follows.upserts) != 1 || follows.upserts[0].Direction != domain.FederationFollowInbound {
		t.Errorf("follows = %+v", follows.upserts)
	}
	if len(client.delivered) != 1 || client.delivered[0] != "https://cairn.example/users/alice#main-key https://remote.example/inbox" {
		t.Errorf("accept delivered = %v", client.delivered)
	}
	// A redelivery of the same id is dropped.
	_ = in.Receive(ctx, deliveryOf(target, map[string]any{"id": "f2", "type": "Follow", "object": "https://cairn.example/users/alice"}))
	if len(follows.upserts) != 1 {
		t.Error("duplicate delivery applied twice")
	}
	if err := in.Receive(ctx, Delivery{Target: target, Body: []byte("{")}); !errors.Is(err, ErrBadActivity) {
		t.Errorf("bad json = %v", err)
	}
}

func TestInbox_CreateOnlyFromFollowed(t *testing.T) {
	in, follows, feed, eng, _, target := inboxFixture()
	ctx := context.Background()
	create := map[string]any{"id": "c1", "type": "Create", "object": map[string]any{"id": "https://remote.example/a/1", "name": "Run"}}
	_ = in.Receive(ctx, deliveryOf(target, create))
	if len(feed.items) != 0 {
		t.Error("stranger's workout ingested")
	}
	follows.following = true
	create["id"] = "c2"
	_ = in.Receive(ctx, deliveryOf(target, create))
	if len(feed.items) != 1 || feed.items[0].Name != "Run" {
		t.Errorf("feed = %+v", feed.items)
	}

	reply := map[string]any{"id": "c3", "type": "Create", "object": map[string]any{
		"id": "https://remote.example/n/1", "content": "<p>nice</p>",
		"inReplyTo": "https://cairn.example/users/alice/activities/" + uuid.NewString(),
	}}
	_ = in.Receive(ctx, deliveryOf(target, reply))
	if eng.comments != 1 {
		t.Errorf("comments = %d", eng.comments)
	}
}

func TestInbox_LikeNeedsPublicAudience(t *testing.T) {
	in, _, _, eng, _, target := inboxFixture()
	ctx := context.Background()
	like := map[string]any{"id": "l1", "type": "Like", "object": "https://cairn.example/users/alice/activities/" + uuid.NewString()}
	in.Audience = closedAudience{}
	_ = in.Receive(ctx, deliveryOf(target, like))
	if eng.kudos != 0 {
		t.Error("kudos on a non-public activity")
	}
	in.Audience = openAudience{}
	like["id"] = "l2"
	_ = in.Receive(ctx, deliveryOf(target, like))
	if eng.kudos != 1 {
		t.Errorf("kudos = %d", eng.kudos)
	}
}

type fakeUsers struct {
	port.UserReader
	port.UserFederationStore
	user    domain.User
	enabled bool
}

func (u fakeUsers) GetUser(context.Context, domain.UserID) (domain.User, error) { return u.user, nil }
func (u fakeUsers) IsFederationEnabled(context.Context, domain.UserID) (bool, error) {
	return u.enabled, nil
}

type fakeDeliveries struct {
	port.FederationDeliveryRepo
	queued      []domain.FederationDelivery
	due         []domain.FederationDelivery
	delivered   int
	dead        []string
	rescheduled int
}

func (d *fakeDeliveries) Enqueue(_ context.Context, row domain.FederationDelivery) error {
	d.queued = append(d.queued, row)
	return nil
}

func (d *fakeDeliveries) ListDue(context.Context, time.Time, int) ([]domain.FederationDelivery, error) {
	return d.due, nil
}

func (d *fakeDeliveries) MarkDelivered(context.Context, domain.FederationDeliveryID) error {
	d.delivered++
	return nil
}

func (d *fakeDeliveries) MarkDead(_ context.Context, _ domain.FederationDeliveryID, reason string) error {
	d.dead = append(d.dead, reason)
	return nil
}

func (d *fakeDeliveries) Reschedule(context.Context, domain.FederationDeliveryID, int, time.Time, string) error {
	d.rescheduled++
	return nil
}

func TestPublisher_QueuesCreateAndDelete(t *testing.T) {
	ctx := context.Background()
	owner := domain.User{ID: domain.UserID(uuid.New()), Username: "alice"}
	follows := &fakeFollows{inboxes: []string{"https://ok.example/inbox", "https://bad.example/inbox"}}
	dels := &fakeDeliveries{}
	p := &Publisher{
		Enabled: true, Instance: testInstance, Users: fakeUsers{user: owner, enabled: true},
		Keys: fakeKeys{}, Follows: follows, Deliveries: dels, Audience: openAudience{},
		Remote: &Remote{Blocks: fakeBlocks{host: "bad.example"}},
	}
	act := domain.Activity{ID: domain.ActivityID(uuid.New()), UserID: owner.ID, Type: domain.ActivityTypeRun}

	p.PublishCreate(ctx, act)
	objID := "https://cairn.example/users/alice/activities/" + act.ID.String()
	if len(dels.queued) != 1 || dels.queued[0].InboxURL != "https://ok.example/inbox" || dels.queued[0].ActivityAPID != objID {
		t.Fatalf("queued = %+v", dels.queued)
	}
	p.PublishDelete(ctx, owner.ID, act.ID)
	if len(dels.queued) != 2 || dels.queued[1].ActivityAPID != objID+"#delete" {
		t.Errorf("delete queued = %+v", dels.queued)
	}

	// Not visible to the public audience: nothing federates.
	p.Audience = closedAudience{}
	p.PublishCreate(ctx, act)
	if len(dels.queued) != 2 {
		t.Error("non-public activity federated")
	}
	p.Audience, p.Enabled = openAudience{}, false
	p.PublishCreate(ctx, act)
	if len(dels.queued) != 2 {
		t.Error("published with federation off")
	}
}

func TestDeliveries_ClassifiesOutcomes(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		status                       int
		inbox                        string
		delivered, dead, rescheduled int
	}{
		{202, "https://ok.example/i", 1, 0, 0},
		{410, "https://ok.example/i", 0, 1, 0},
		{429, "https://ok.example/i", 0, 0, 1},
		{503, "https://ok.example/i", 0, 0, 1},
		{202, "https://bad.example/i", 0, 1, 0},
	} {
		repo := &fakeDeliveries{due: []domain.FederationDelivery{{InboxURL: tc.inbox, MaxAttempts: 5}}}
		d := &Deliveries{
			Repo: repo, Remote: &Remote{Blocks: fakeBlocks{host: "bad.example"}},
			Sender: &Sender{Keys: fakeKeys{}, Client: &fakeClient{status: tc.status}},
		}
		d.ProcessDue(ctx)
		if repo.delivered != tc.delivered || len(repo.dead) != tc.dead || repo.rescheduled != tc.rescheduled {
			t.Errorf("status %d to %s: delivered %d dead %v rescheduled %d", tc.status, tc.inbox, repo.delivered, repo.dead, repo.rescheduled)
		}
	}
	// The last attempt gives up instead of rescheduling.
	repo := &fakeDeliveries{due: []domain.FederationDelivery{{InboxURL: "https://ok.example/i", Attempts: 4, MaxAttempts: 5}}}
	(&Deliveries{Repo: repo, Sender: &Sender{Keys: fakeKeys{}, Client: &fakeClient{status: 500}}}).ProcessDue(ctx)
	if len(repo.dead) != 1 || repo.rescheduled != 0 {
		t.Errorf("max attempts: dead %v rescheduled %d", repo.dead, repo.rescheduled)
	}
}
