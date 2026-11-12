/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package nodestatus

import (
	"context"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Node lifecycle condition types from KEP-5683. They live in k8s.io/api/core/v1 from v1.37;
// v1.36 doesn't have them, so they're defined here.
const (
	NodeGracefulNodeShutdownInProgress v1.NodeConditionType = "GracefulNodeShutdownInProgress"
	NodeDrainInProgress                v1.NodeConditionType = "DrainInProgress"
)

const (
	nodeShutdownReason         = "NodeShutdown"
	nodeShutdownMessage        = "Kubelet received a shutdown signal and is terminating pods"
	nodeShutdownDrainMessage   = "Graceful node shutdown is draining pods on this node"
	nodeNotShuttingDownReason  = "NodeNotShuttingDown"
	nodeNotShuttingDownMessage = "Kubelet is not in graceful node shutdown"
)

// GracefulNodeShutdownConditions returns a Setter that publishes KEP-6249's
// GracefulNodeShutdownInProgress and DrainInProgress conditions from the shutdown manager's state.
//
// While the node is shutting down, GracefulNodeShutdownInProgress is True. DrainInProgress is set
// True too, unless another writer already had it True when the shutdown started; then it's left
// alone. Otherwise GracefulNodeShutdownInProgress is set False if it's True (shutdown cancelled,
// kubelet startup, or someone else wrote it), and DrainInProgress is set False only if this kubelet
// wrote it. Conditions are never removed, and an absent condition isn't created just to say False.
//
// Which DrainInProgress this kubelet wrote is kept in memory, so a restarted kubelet leaves
// DrainInProgress alone (KEP-6249 records it in the shutdown state file instead).
func GracefulNodeShutdownConditions(nowFunc func() time.Time, // typically Kubelet.clock.Now
	shutdownStatusFunc func() error, // typically Kubelet.shutdownManager.ShutdownStatus
) Setter {
	var mu sync.Mutex
	inShutdown, ownsDrain := false, false
	return func(ctx context.Context, node *v1.Node) error {
		mu.Lock()
		defer mu.Unlock()
		now := metav1.NewTime(nowFunc())
		shuttingDown := shutdownStatusFunc() != nil
		if shuttingDown && !inShutdown {
			ownsDrain = !conditionIsTrue(node, NodeDrainInProgress)
		}
		inShutdown = shuttingDown

		if shuttingDown {
			setNodeCondition(node, NodeGracefulNodeShutdownInProgress, v1.ConditionTrue, nodeShutdownReason, nodeShutdownMessage, now)
			if ownsDrain {
				setNodeCondition(node, NodeDrainInProgress, v1.ConditionTrue, nodeShutdownReason, nodeShutdownDrainMessage, now)
			}
			return nil
		}
		if conditionIsTrue(node, NodeGracefulNodeShutdownInProgress) {
			setNodeCondition(node, NodeGracefulNodeShutdownInProgress, v1.ConditionFalse, nodeNotShuttingDownReason, nodeNotShuttingDownMessage, now)
		}
		if ownsDrain {
			setNodeCondition(node, NodeDrainInProgress, v1.ConditionFalse, nodeNotShuttingDownReason, nodeNotShuttingDownMessage, now)
			ownsDrain = false
		}
		return nil
	}
}

func conditionIsTrue(node *v1.Node, conditionType v1.NodeConditionType) bool {
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == conditionType {
			return node.Status.Conditions[i].Status == v1.ConditionTrue
		}
	}
	return false
}

// setNodeCondition sets a condition's status, adding it if it's missing. LastTransitionTime
// changes only when the status does.
func setNodeCondition(node *v1.Node, conditionType v1.NodeConditionType, status v1.ConditionStatus, reason, message string, now metav1.Time) {
	for i := range node.Status.Conditions {
		c := &node.Status.Conditions[i]
		if c.Type != conditionType {
			continue
		}
		if c.Status != status {
			c.LastTransitionTime = now
		}
		c.Status, c.Reason, c.Message, c.LastHeartbeatTime = status, reason, message, now
		return
	}
	node.Status.Conditions = append(node.Status.Conditions, v1.NodeCondition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastHeartbeatTime:  now,
		LastTransitionTime: now,
	})
}
