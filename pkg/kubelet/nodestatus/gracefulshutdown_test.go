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
	"errors"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type gnsStep struct {
	shuttingDown bool
	edit         func(node *v1.Node) // applied to the node before the setter runs, e.g. another writer
	want         map[v1.NodeConditionType]string
}

func TestGracefulNodeShutdownConditions(t *testing.T) {
	drainByOther := func(node *v1.Node) {
		node.Status.Conditions = append(node.Status.Conditions, v1.NodeCondition{
			Type: NodeDrainInProgress, Status: v1.ConditionTrue, Reason: "KubectlDrain",
		})
	}
	removeGNS := func(node *v1.Node) {
		var kept []v1.NodeCondition
		for _, c := range node.Status.Conditions {
			if c.Type != NodeGracefulNodeShutdownInProgress {
				kept = append(kept, c)
			}
		}
		node.Status.Conditions = kept
	}
	cases := map[string][]gnsStep{
		"no shutdown adds nothing": {
			{want: map[v1.NodeConditionType]string{}},
		},
		"shutdown sets both, cancel clears both": {
			{want: map[v1.NodeConditionType]string{}},
			{shuttingDown: true, want: map[v1.NodeConditionType]string{
				NodeGracefulNodeShutdownInProgress: "True/NodeShutdown", NodeDrainInProgress: "True/NodeShutdown"}},
			{shuttingDown: true, want: map[v1.NodeConditionType]string{
				NodeGracefulNodeShutdownInProgress: "True/NodeShutdown", NodeDrainInProgress: "True/NodeShutdown"}},
			{want: map[v1.NodeConditionType]string{
				NodeGracefulNodeShutdownInProgress: "False/NodeNotShuttingDown", NodeDrainInProgress: "False/NodeNotShuttingDown"}},
		},
		"drain set by another writer is left alone": {
			{edit: drainByOther, want: map[v1.NodeConditionType]string{NodeDrainInProgress: "True/KubectlDrain"}},
			{shuttingDown: true, want: map[v1.NodeConditionType]string{
				NodeGracefulNodeShutdownInProgress: "True/NodeShutdown", NodeDrainInProgress: "True/KubectlDrain"}},
			{want: map[v1.NodeConditionType]string{
				NodeGracefulNodeShutdownInProgress: "False/NodeNotShuttingDown", NodeDrainInProgress: "True/KubectlDrain"}},
		},
		"startup clears a stale shutdown condition but not drain": {
			{edit: func(node *v1.Node) {
				node.Status.Conditions = []v1.NodeCondition{
					{Type: NodeGracefulNodeShutdownInProgress, Status: v1.ConditionTrue, Reason: nodeShutdownReason},
					{Type: NodeDrainInProgress, Status: v1.ConditionTrue, Reason: nodeShutdownReason},
				}
			}, want: map[v1.NodeConditionType]string{
				NodeGracefulNodeShutdownInProgress: "False/NodeNotShuttingDown", NodeDrainInProgress: "True/NodeShutdown"}},
		},
		"condition removed mid-shutdown is restored": {
			{shuttingDown: true, want: map[v1.NodeConditionType]string{
				NodeGracefulNodeShutdownInProgress: "True/NodeShutdown", NodeDrainInProgress: "True/NodeShutdown"}},
			{shuttingDown: true, edit: removeGNS, want: map[v1.NodeConditionType]string{
				NodeGracefulNodeShutdownInProgress: "True/NodeShutdown", NodeDrainInProgress: "True/NodeShutdown"}},
		},
	}
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			shuttingDown := false
			setter := GracefulNodeShutdownConditions(time.Now, func() error {
				if shuttingDown {
					return errors.New("node is shutting down")
				}
				return nil
			})
			node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}}
			for i, step := range steps {
				shuttingDown = step.shuttingDown
				if step.edit != nil {
					step.edit(node)
				}
				if err := setter(context.Background(), node); err != nil {
					t.Fatalf("step %d: %v", i, err)
				}
				got := map[v1.NodeConditionType]string{}
				for _, c := range node.Status.Conditions {
					got[c.Type] = string(c.Status) + "/" + c.Reason
				}
				if len(got) != len(step.want) {
					t.Fatalf("step %d: got %v, want %v", i, got, step.want)
				}
				for k, v := range step.want {
					if got[k] != v {
						t.Fatalf("step %d: %s = %q, want %q (all: %v)", i, k, got[k], v, got)
					}
				}
			}
		})
	}
}
