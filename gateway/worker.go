package gateway

import "time"

type Worker struct {
	ID      string
	Model   string
	Address string

	ActiveRequests  int
	WaitingRequests int

	GPUUtil     float64
	KVCacheUtil float64

	LastHeartbeat time.Time
}
