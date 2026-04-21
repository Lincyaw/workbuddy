package app

import "time"

// Default tuning constants shared by the serve and coordinator topologies.
// Keeping them here (rather than in cmd/) lets tests drive the assembly
// without importing the Cobra command package.
const (
	DefaultPort              = 8080
	DefaultPollInterval      = 30 * time.Second
	TaskChanSize             = 64
	AgentShutdownWait        = 60 * time.Second
	DefaultWorkerHeartbeat   = 15 * time.Second
	DefaultLongPollTimeout   = 30 * time.Second
	LongPollCheckInterval    = 100 * time.Millisecond
	ConfigReloadDebounce     = 200 * time.Millisecond
)
