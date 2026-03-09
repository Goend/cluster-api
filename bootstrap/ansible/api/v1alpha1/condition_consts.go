/*
Copyright 2021 The Kubernetes Authors.

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

package v1alpha1

import clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

// Conditions and condition Reasons for the AnsibleConfig object.

const (
	// DataSecretAvailableCondition documents the status of the bootstrap secret generation process.
	//
	// NOTE: When the DataSecret generation starts the process completes immediately and within the
	// same reconciliation, so the user will always see a transition from Wait to Generated without having
	// evidence that BootstrapSecret generation is started/in progress.
	DataSecretAvailableCondition clusterv1.ConditionType = "DataSecretAvailable"

	// DataSecretGeneratedReason indicates a bootstrap data secret has been successfully created.
	DataSecretGeneratedReason = "DataSecretGenerated"

	// WaitingForClusterInfrastructureReason (Severity=Info) document a bootstrap secret generation process
	// waiting for the cluster infrastructure to be ready.
	//
	// NOTE: Having the cluster infrastructure ready is a pre-condition for starting to create machines;
	// the AnsibleConfig controller ensure this pre-condition is satisfied.
	WaitingForClusterInfrastructureReason = "WaitingForClusterInfrastructure"

	// DataSecretGenerationFailedReason (Severity=Warning) documents a AnsibleConfig controller detecting
	// an error while generating a data secret; those kind of errors are usually due to misconfigurations
	// and user intervention is required to get them fixed.
	DataSecretGenerationFailedReason = "DataSecretGenerationFailed"
	// SSHConnectivityCondition documents whether the target machine is reachable via SSH using the bastion proxy.
	SSHConnectivityCondition clusterv1.ConditionType = "SSHConnectivity"

	// SSHReachableReason indicates SSH connectivity probe succeeded.
	SSHReachableReason = "SSHReachable"
	// SSHUnreachableReason indicates SSH probe failed to reach the target.
	SSHUnreachableReason = "SSHUnreachable"
	// SSHProbeErrorReason indicates an unexpected error occurred during probe.
	SSHProbeErrorReason = "SSHProbeError"
	// BastionNotFoundReason indicates the bastion Floating IP could not be resolved.
	BastionNotFoundReason = "BastionNotFound"
)
