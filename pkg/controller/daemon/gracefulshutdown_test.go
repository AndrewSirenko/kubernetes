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

package daemon

import (
	"testing"
	"time"

	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/kubernetes/pkg/controller/daemon/util"
)

func setNodeCondition(node *v1.Node, conditionType v1.NodeConditionType, status v1.ConditionStatus) {
	node.Status.Conditions = append(node.Status.Conditions, v1.NodeCondition{Type: conditionType, Status: status})
}

func markShuttingDown(t *testing.T, manager *daemonSetsController, name string) {
	t.Helper()
	obj, exists, err := manager.nodeStore.GetByKey(name)
	if err != nil || !exists {
		t.Fatalf("node %s: exists=%v err=%v", name, exists, err)
	}
	node := obj.(*v1.Node).DeepCopy()
	setNodeCondition(node, NodeGracefulNodeShutdownInProgress, v1.ConditionTrue)
	if err := manager.nodeStore.Update(node); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownSuppressedNodeGetsNoNewPods(t *testing.T) {
	for _, strategy := range updateStrategies() {
		ds := newDaemonSet("foo")
		ds.Spec.UpdateStrategy = *strategy
		_, ctx := ktesting.NewTestContext(t)
		manager, podControl, _, err := newTestController(ctx, ds)
		if err != nil {
			t.Fatalf("error creating DaemonSets controller: %v", err)
		}
		addNodes(manager.nodeStore, 0, 5, nil)
		markShuttingDown(t, manager, "node-0")
		// DrainInProgress alone (kubectl drain, a maintenance operator) does not suppress.
		node1, _, _ := manager.nodeStore.GetByKey("node-1")
		drained := node1.(*v1.Node).DeepCopy()
		setNodeCondition(drained, "DrainInProgress", v1.ConditionTrue)
		if err := manager.nodeStore.Update(drained); err != nil {
			t.Fatal(err)
		}
		// False means no shutdown in progress.
		node2, _, _ := manager.nodeStore.GetByKey("node-2")
		cleared := node2.(*v1.Node).DeepCopy()
		setNodeCondition(cleared, NodeGracefulNodeShutdownInProgress, v1.ConditionFalse)
		if err := manager.nodeStore.Update(cleared); err != nil {
			t.Fatal(err)
		}
		if err := manager.dsStore.Add(ds); err != nil {
			t.Fatal(err)
		}
		expectSyncDaemonSets(t, manager, ds, podControl, 4, 0, 0)
		for _, template := range podControl.Templates {
			if nodeName, _ := util.GetTargetNodeName(&v1.Pod{Spec: template.Spec}); nodeName == "node-0" {
				t.Fatalf("created a pod on node-0, which is shutting down")
			}
		}
	}
}

func TestShutdownSuppressedNodeStillDeletesFailedPods(t *testing.T) {
	tests := []struct {
		name                                             string
		numFailedPods, numRunningPods                    int
		expectedCreates, expectedDeletes, expectedEvents int
	}{
		{name: "failed pod is deleted and not replaced", numFailedPods: 1, expectedDeletes: 1, expectedEvents: 1},
		{name: "excess running pods are left to the kubelet", numRunningPods: 3},
		{name: "failed pod deleted, running pods kept", numFailedPods: 1, numRunningPods: 2, expectedDeletes: 1, expectedEvents: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, strategy := range updateStrategies() {
				ds := newDaemonSet("foo")
				ds.Spec.UpdateStrategy = *strategy
				_, ctx := ktesting.NewTestContext(t)
				manager, podControl, _, err := newTestController(ctx, ds)
				if err != nil {
					t.Fatalf("error creating DaemonSets controller: %v", err)
				}
				if err := manager.dsStore.Add(ds); err != nil {
					t.Fatal(err)
				}
				addNodes(manager.nodeStore, 0, 1, nil)
				markShuttingDown(t, manager, "node-0")
				addFailedPods(manager.podStore, "node-0", simpleDaemonSetLabel, ds, test.numFailedPods)
				addPods(manager.podStore, "node-0", simpleDaemonSetLabel, ds, test.numRunningPods)
				expectSyncDaemonSets(t, manager, ds, podControl, test.expectedCreates, test.expectedDeletes, test.expectedEvents)
			}
		})
	}
}

func TestRollingUpdateSkipsShutdownSuppressedNode(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ds := newDaemonSet("foo")
	manager, podControl, _, err := newTestController(ctx, ds)
	if err != nil {
		t.Fatalf("error creating DaemonSets controller: %v", err)
	}
	addNodes(manager.nodeStore, 0, 2, nil)
	if err := manager.dsStore.Add(ds); err != nil {
		t.Fatal(err)
	}
	expectSyncDaemonSets(t, manager, ds, podControl, 2, 0, 0)
	markPodsReady(podControl.podStore)
	markShuttingDown(t, manager, "node-0")

	ds.Spec.Template.Spec.Containers[0].Image = "foo2/bar2"
	ds.Spec.UpdateStrategy.Type = apps.RollingUpdateDaemonSetStrategyType
	maxUnavailable := intstr.FromInt32(2)
	ds.Spec.UpdateStrategy.RollingUpdate = &apps.RollingUpdateDaemonSet{MaxUnavailable: &maxUnavailable}
	if err := manager.dsStore.Update(ds); err != nil {
		t.Fatal(err)
	}
	clearExpectations(t, manager, ds, podControl)
	// Only node-1's old pod is replaced; node-0's belongs to the kubelet's shutdown.
	expectSyncDaemonSets(t, manager, ds, podControl, 0, 1, 0)
}

func TestShouldIgnoreNodeUpdateShutdownCondition(t *testing.T) {
	withCondition := func(status v1.ConditionStatus, heartbeat time.Time) *v1.Node {
		node := newNode("node1", nil)
		node.Status.Conditions = []v1.NodeCondition{{
			Type: NodeGracefulNodeShutdownInProgress, Status: status, LastHeartbeatTime: metav1.NewTime(heartbeat),
		}}
		return node
	}
	t0 := time.Now()
	cases := []struct {
		name     string
		old, cur *v1.Node
		ignore   bool
	}{
		{"no condition", newNode("node1", nil), newNode("node1", nil), true},
		{"enter", newNode("node1", nil), withCondition(v1.ConditionTrue, t0), false},
		{"exit", withCondition(v1.ConditionTrue, t0), withCondition(v1.ConditionFalse, t0), false},
		{"heartbeat only", withCondition(v1.ConditionTrue, t0), withCondition(v1.ConditionTrue, t0.Add(time.Minute)), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldIgnoreNodeUpdate(*c.old, *c.cur); got != c.ignore {
				t.Errorf("shouldIgnoreNodeUpdate = %v, want %v", got, c.ignore)
			}
		})
	}
}
