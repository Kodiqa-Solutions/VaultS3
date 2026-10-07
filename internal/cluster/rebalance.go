package cluster

import (
	"log/slog"
	"sync"

	"github.com/Kodiqa-Solutions/VaultS3/internal/config"
	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
	"github.com/Kodiqa-Solutions/VaultS3/internal/storage"
)

// Rebalancer used to move objects to their hash-ring primary after a membership
// change. It no longer moves anything, and replica repair does that job.
//
// The old pass was wrong in both of the ways it could run. As shipped it sent
// each object to the new owner as an unsigned S3 PUT, which every node refuses
// with 403, after reading the whole object into memory, for every object this
// node held but was not primary for. With three replicas that is two thirds of
// a node's data, and the failure detector started a pass on every down and
// recover transition, so the cost was real and nothing ever moved. Had the PUT
// been accepted it would have been worse: the pass then deleted the local copy,
// which on a secondary holder is a legitimate replica, and called the Raft
// backed DeleteObjectMeta, which removes the object from the whole cluster.
// While it ran it also held off replica repair, the one mechanism that places
// copies correctly.
//
// The type and its methods stay so the admin API and CLI endpoint keep
// answering, now with a message saying what to use instead.
type Rebalancer struct {
	once sync.Once
}

// RebalanceConfig is an alias for config.RebalanceConfig.
type RebalanceConfig = config.RebalanceConfig

// RebalanceRetiredMessage is what a rebalance request is answered with.
const RebalanceRetiredMessage = "rebalance no longer moves data: replica repair restores every object's " +
	"placement and copy count (POST /api/v1/cluster/repair, or wait for the next scheduled scan)"

// NewRebalancer creates the retired rebalancer. The arguments are accepted and
// ignored so existing callers keep compiling.
func NewRebalancer(
	_ metadata.StoreAPI,
	_ storage.Engine,
	_ *HashRing,
	_ *Proxy,
	_ string,
	_ RebalanceConfig,
) *Rebalancer {
	return &Rebalancer{}
}

// Trigger does nothing but say, once per process, that replica repair handles
// placement.
func (r *Rebalancer) Trigger() {
	r.once.Do(func() {
		slog.Info("rebalance: " + RebalanceRetiredMessage)
	})
}

// Stop is a no-op, kept for callers.
func (r *Rebalancer) Stop() {}

// IsRunning is always false: there is nothing to run.
func (r *Rebalancer) IsRunning() bool { return false }

// RebalanceStatus reports the current state.
type RebalanceStatus struct {
	Running bool   `json:"running"`
	Message string `json:"message,omitempty"`
}

// Status reports that rebalance is retired.
func (r *Rebalancer) Status() RebalanceStatus {
	return RebalanceStatus{Running: false, Message: RebalanceRetiredMessage}
}
