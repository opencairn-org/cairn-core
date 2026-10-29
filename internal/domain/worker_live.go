package domain

import "time"

// LiveCapability is what a worker reports it can do for one data type.
type LiveCapability struct {
	Read        bool
	Write       bool
	Backfill    bool
	Granularity string
}

// WorkerHeartbeat is one worker instance's presence heartbeat as the admin sees it.
type WorkerHeartbeat struct {
	WorkerName   string
	WorkerKey    string
	InstanceID   string
	Version      string
	Release      string
	Compatible   bool // release matches core's major.minor (or either is untagged)
	Known        bool // release could be parsed
	Provider     string
	Package      string
	GoVersion    string
	Commit       string
	BuildDate    string
	Webhooks     bool
	Capabilities map[string]LiveCapability
	LastSeen     time.Time
}

// PoolStatus is a registered worker pool with its live instance count.
type PoolStatus struct {
	WorkerPool
	OnlineInstances int
}
