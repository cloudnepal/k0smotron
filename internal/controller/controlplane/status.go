/*
Copyright 2024.

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

package controlplane

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/remote"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/collections"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cpv1beta1 "github.com/k0sproject/k0smotron/api/controlplane/v1beta1"
	"github.com/k0sproject/version"
)

func versionMatches(machine *clusterv1.Machine, ver string) bool {

	if machine.Spec.Version == nil || *machine.Spec.Version == "" {
		return false
	}

	if *machine.Spec.Version == ver {
		return true
	}

	machineVersion := *machine.Spec.Version
	kcpVersion := ver

	// If either of the versions is missing the suffix, we need to add it
	if !strings.Contains(machineVersion, "+") {
		machineVersion = machineVersion + "+k0s.0" // FIXME Use constants
	}
	if !strings.Contains(kcpVersion, "+") {
		kcpVersion = kcpVersion + "+k0s.0" // FIXME Use constants
	}

	// Compare the versions
	vMachine := version.MustParse(machineVersion)
	vKCP := version.MustParse(kcpVersion)

	return vKCP.Equal(vMachine)

}

func computeStatus(machines collections.Machines, kcp *cpv1beta1.K0sControlPlane) {
	kcp.Status.Replicas = int32(len(machines))
	readyReplicas := 0
	updatedReplicas := 0
	unavailableReplicas := 0
	versions := []string{}
	// Count the machines in different states
	for _, machine := range machines {
		// TODO Do we need to distinguish between Running and Provisioned
		// when we run ctrl node without --enable-worker?
		switch machine.Status.Phase {
		case string(clusterv1.MachinePhaseRunning):
			readyReplicas++
		case string(clusterv1.MachinePhaseDeleting), string(clusterv1.MachinePhaseDeleted):
			// Do nothing
		default:
			unavailableReplicas++
		}

		if versionMatches(machine, kcp.Spec.Version) {
			updatedReplicas++
		}
		versions = append(versions, *machine.Spec.Version)
	}

	kcp.Status.ReadyReplicas = int32(readyReplicas)
	kcp.Status.UpdatedReplicas = int32(updatedReplicas)
	kcp.Status.UnavailableReplicas = int32(unavailableReplicas)

	// Find the lowest version
	if len(versions) > 0 {
		v, err := version.NewCollection(versions...)
		if err != nil {
			// TODO Set maybe some conditions or something
			log.Log.Error(err, "Failed to parse versions")
		} else {
			sort.Sort(v)
			kcp.Status.Version = v[0].String()
		}
	}

	// If the controlplane spec does NOT have workers enabled
	// we need to mark the controlplane as externally managed
	// Otherwise CAPI assumes it'll find node objects for the machines
	if !slices.Contains(kcp.Spec.K0sConfigSpec.Args, "--enable-worker") {
		kcp.Status.ExternalManagedControlPlane = true
	}

}

func (c *K0sController) updateStatus(ctx context.Context, kcp *cpv1beta1.K0sControlPlane, cluster *clusterv1.Cluster) error {
	logger := log.FromContext(ctx)

	selector := collections.ControlPlaneSelectorForCluster(cluster.Name)
	kcp.Status.Selector = selector.String()

	// Collect the facts: machines, child cluster status etc. to "calculate" the status and conditions

	machines, err := collections.GetFilteredMachinesForCluster(ctx, c.Client, cluster, collections.ControlPlaneMachines(cluster.Name))
	if err != nil {
		return fmt.Errorf("failed to get machines: %w", err)
	}

	computeStatus(machines, kcp)
	kcp.Status.Ready = false
	logger.Info("Computed status", "status", kcp.Status)
	// Check if the control plane is ready by connecting to the API server
	// and checking if the control plane is initialized
	logger.Info("Pinging the workload cluster API")
	// Get the CAPI cluster accessor
	client, err := remote.NewClusterClient(ctx, "", c.Client, util.ObjectKey(cluster))
	if err != nil {
		logger.Info("Failed to create cluster client", "error", err)
		// Set a condition for this so we can determine later if we should requeue the reconciliation
		conditions.MarkFalse(kcp, cpv1beta1.ControlPlaneReadyCondition, "Unable to connect to the workload cluster API", clusterv1.ConditionSeverityWarning, "Failed to create cluster client: %v", err)
		return nil
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// If we can get 'kube-system' namespace, it's safe to say the API is up-and-running
	ns := &corev1.Namespace{}
	nsKey := types.NamespacedName{
		Namespace: "",
		Name:      "kube-system",
	}
	err = client.Get(pingCtx, nsKey, ns)
	if err != nil {
		conditions.MarkFalse(kcp, cpv1beta1.ControlPlaneReadyCondition, "Unable to connect to the workload cluster API", clusterv1.ConditionSeverityWarning, "Failed to get namespace: %v", err)
		return nil
	}

	// Set the conditions
	conditions.MarkTrue(kcp, cpv1beta1.ControlPlaneReadyCondition)
	kcp.Status.Ready = true
	kcp.Status.ControlPlaneReady = true
	kcp.Status.Inititalized = true

	return nil

}
