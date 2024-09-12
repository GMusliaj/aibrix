/*
Copyright 2024 The Aibrix Team.

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

package rayclusterreplicaset

import (
	"reflect"

	orchestrationv1alpha1 "github.com/aibrix/aibrix/api/orchestration/v1alpha1"
	rayclusterutil "github.com/aibrix/aibrix/pkg/utils"
	rayclusterv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
)

// NewCondition creates a new replicaset condition.
func NewCondition(condType string, status metav1.ConditionStatus, reason, msg string) metav1.Condition {
	return metav1.Condition{
		Type:               condType,
		Status:             status,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            msg,
	}
}

// GetCondition returns a replicaset condition with the provided type if it exists.
func GetCondition(status orchestrationv1alpha1.RayClusterReplicaSetStatus, condType string) *metav1.Condition {
	for _, c := range status.Conditions {
		if c.Type == condType {
			return &c
		}
	}
	return nil
}

// SetCondition adds/replaces the given condition in the replicaset status. If the condition that we
// are about to add already exists and has the same status and reason then we are not going to update.
func SetCondition(status *orchestrationv1alpha1.RayClusterReplicaSetStatus, condition metav1.Condition) {
	currentCond := GetCondition(*status, condition.Type)
	if currentCond != nil && currentCond.Status == condition.Status && currentCond.Reason == condition.Reason {
		return
	}
	newConditions := filterOutCondition(status.Conditions, condition.Type)
	status.Conditions = append(newConditions, condition)
}

// RemoveCondition removes the condition with the provided type from the replicaset status.
func RemoveCondition(status *orchestrationv1alpha1.RayClusterReplicaSetStatus, condType string) {
	status.Conditions = filterOutCondition(status.Conditions, condType)
}

// filterOutCondition returns a new slice of replicaset conditions without conditions with the provided type.
func filterOutCondition(conditions []metav1.Condition, condType string) []metav1.Condition {
	var newConditions []metav1.Condition
	for _, c := range conditions {
		if c.Type == condType {
			continue
		}
		newConditions = append(newConditions, c)
	}
	return newConditions
}

// Helper function to construct a new Pod from a ReplicaSet
func constructRayCluster(replicaset *orchestrationv1alpha1.RayClusterReplicaSet) *rayclusterv1.RayCluster {
	cluster := &rayclusterv1.RayCluster{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: replicaset.Name + "-",
			Namespace:    replicaset.Namespace,
			Labels:       replicaset.Spec.Selector.MatchLabels,
			Annotations:  make(map[string]string),
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(replicaset, controllerKind),
			},
		},
		Spec: replicaset.Spec.Template,
	}

	return cluster
}

// filterActiveClusters filters out inactive Cluster from a list of RayClusters
func filterActiveClusters(clusters []rayclusterv1.RayCluster) []rayclusterv1.RayCluster {
	var activeClusters []rayclusterv1.RayCluster
	for _, cluster := range clusters {
		if isClusterActive(cluster) {
			activeClusters = append(activeClusters, cluster)
		} else {
			klog.V(4).Info("Ignore inactive cluster", "name", cluster.Name, "state", cluster.Status.State, "deletionTime", cluster.DeletionTimestamp)
		}

	}

	return activeClusters
}

func isClusterActive(c rayclusterv1.RayCluster) bool {
	// TODO: Should we validate cluster state?
	return c.DeletionTimestamp != nil
}

func isStatusSame(rs *orchestrationv1alpha1.RayClusterReplicaSet, newStatus orchestrationv1alpha1.RayClusterReplicaSetStatus) bool {
	// Only update the status if something has actually changed
	if rs.Status.Replicas == newStatus.Replicas &&
		rs.Status.FullyLabeledReplicas == newStatus.FullyLabeledReplicas &&
		rs.Status.ReadyReplicas == newStatus.ReadyReplicas &&
		rs.Status.AvailableReplicas == newStatus.AvailableReplicas &&
		rs.Generation == rs.Status.ObservedGeneration &&
		reflect.DeepEqual(rs.Status.Conditions, newStatus.Conditions) {
		return true
	}

	return false
}

func calculateStatus(rs *orchestrationv1alpha1.RayClusterReplicaSet, filteredClusters []rayclusterv1.RayCluster, manageReplicasErr error) orchestrationv1alpha1.RayClusterReplicaSetStatus {
	newStatus := rs.Status
	// Count the number of pods that have labels matching the labels of the cluster
	// template of the replica set, the matching pods may have more
	// labels than are in the template. Because the label of podTemplateSpec is
	// a superset of the selector of the replica set, so the possible
	// matching pods must be part of the filteredClusters.
	fullyLabeledReplicasCount := 0
	readyReplicasCount := 0
	availableReplicasCount := 0
	templateLabel := labels.Set(rs.Spec.Selector.MatchLabels).AsSelectorPreValidated()
	for _, cluster := range filteredClusters {
		if templateLabel.Matches(labels.Set(cluster.Labels)) {
			fullyLabeledReplicasCount++
		}
		if rayclusterutil.IsRayClusterReady(&cluster) {
			readyReplicasCount++
			if rayclusterutil.IsRayClusterAvailable(&cluster, rs.Spec.MinReadySeconds, metav1.Now()) {
				availableReplicasCount++
			}
		}
	}

	failureCond := GetCondition(rs.Status, orchestrationv1alpha1.RayClusterReplicaSetReplicaFailure)
	if manageReplicasErr != nil && failureCond == nil {
		var reason string
		if diff := len(filteredClusters) - int(*(rs.Spec.Replicas)); diff < 0 {
			reason = "FailedCreate"
		} else if diff > 0 {
			reason = "FailedDelete"
		}
		cond := NewCondition(orchestrationv1alpha1.RayClusterReplicaSetReplicaFailure, metav1.ConditionTrue, reason, manageReplicasErr.Error())
		SetCondition(&newStatus, cond)
	} else if manageReplicasErr == nil && failureCond != nil {
		RemoveCondition(&newStatus, orchestrationv1alpha1.RayClusterReplicaSetReplicaFailure)
	}

	newStatus.Replicas = int32(len(filteredClusters))
	newStatus.FullyLabeledReplicas = int32(fullyLabeledReplicasCount)
	newStatus.ReadyReplicas = int32(readyReplicasCount)
	newStatus.AvailableReplicas = int32(availableReplicasCount)

	return newStatus
}
