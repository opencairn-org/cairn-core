package domain

import "time"

// RemoteKudoser is a federated actor who liked an activity over ActivityPub.
type RemoteKudoser struct {
	ActorID string
	Handle  string // @user@domain
}

// RemoteComment is a federated reply (Note) on an activity.
type RemoteComment struct {
	ID        string // the Note's ActivityPub id
	ActorID   string
	Handle    string // @user@domain
	Body      string
	CreatedAt time.Time
}
