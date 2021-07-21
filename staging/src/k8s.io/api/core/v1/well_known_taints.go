/*
Copyright 2019 The Kubernetes Authors.

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

package v1

const (
	// TaintNodeNotReady will be added when node is not ready
	// and removed when node becomes ready.
	TaintNodeNotReady = "node.kubernetes.io/not-ready" //1. (node-lifecycle-controller)当node.NodeReady condition为false时会打上这个taint,Effect为NoSchedule

	// TaintNodeUnreachable will be added when node becomes unreachable
	// (corresponding to NodeReady status ConditionUnknown)
	// and removed when node becomes reachable (NodeReady status ConditionTrue).
	TaintNodeUnreachable = "node.kubernetes.io/unreachable" //1. (node-lifecycle-controller)当node.NodeReady condition为unknown时会打上这个taint,Effect为NoSchedule

	// TaintNodeUnschedulable will be added when node becomes unschedulable
	// and removed when node becomes scheduable.
	TaintNodeUnschedulable = "node.kubernetes.io/unschedulable" //1. (node-lifecycle-controller)当node.spec.Unschedulable为true时会打上这个taint,Effect为NoSchedule

	// TaintNodeMemoryPressure will be added when node has memory pressure
	// and removed when node has enough memory.
	TaintNodeMemoryPressure = "node.kubernetes.io/memory-pressure" //1. (node-lifecycle-controller)当node.MemoryPressure condition为true时会打上这个taint,Effect为NoSchedule

	// TaintNodeDiskPressure will be added when node has disk pressure
	// and removed when node has enough disk.
	TaintNodeDiskPressure = "node.kubernetes.io/disk-pressure" //1. (node-lifecycle-controller)当node.DiskPressure condition为true时会打上这个taint,Effect为NoSchedule

	// TaintNodeNetworkUnavailable will be added when node's network is unavailable
	// and removed when network becomes ready.
	TaintNodeNetworkUnavailable = "node.kubernetes.io/network-unavailable" //1. (node-lifecycle-controller)当node.NetworkUnavailable condition为true时会打上这个taint,Effect为NoSchedule

	// TaintNodePIDPressure will be added when node has pid pressure
	// and removed when node has enough disk.
	TaintNodePIDPressure = "node.kubernetes.io/pid-pressure" //1. (node-lifecycle-controller)当node.PIDPressure condition为true时会打上这个taint,Effect为NoSchedule
)
