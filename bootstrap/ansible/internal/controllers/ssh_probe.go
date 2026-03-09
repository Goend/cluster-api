package controllers

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/ansible/api/v1alpha1"

	sshlib "golang.org/x/crypto/ssh"
)

// ensureSSHConnectivity checks SSH reachability to the target machine via bastion proxy using the "<cluster>-ssh-auth" Secret.
// Returns (ok, reason, message, err). If err != nil caller should requeue with backoff.
func ensureSSHConnectivity(ctx context.Context, c client.Client, scope *Scope) (bool, string, string, error) {
	// Resolve bastion Floating IP
	fip, err := (&AnsibleConfigReconciler{Client: c}).bastionFIP(ctx, scope)
	if err != nil {
		return false, bootstrapv1.SSHProbeErrorReason, fmt.Sprintf("resolve bastion FIP failed: %v", err), nil
	}
	if fip == "" {
		return false, bootstrapv1.BastionNotFoundReason, "bastion floating IP not available", nil
	}

	// Resolve target machine IP
	machine, err := machineFromScope(scope)
	if err != nil || machine == nil {
		return false, bootstrapv1.SSHProbeErrorReason, "cannot resolve machine from scope", err
	}
	// 优先按 Scope.PreferredCIDR 过滤选择；失败时回退
	targetIP, err := selectMachineIPAddress(machine, scope.PreferredCIDR)
	if err != nil || targetIP == "" {
		return false, bootstrapv1.SSHProbeErrorReason, "machine has no usable IP address", nil
	}

	// Load SSH private key from <cluster>-ssh-auth Secret
	secretName := kubeanSSHAuthSecretName(scope)
	ns := scope.Config.Namespace
	key := client.ObjectKey{Namespace: ns, Name: secretName}
	sec := &corev1.Secret{}
	if err := c.Get(ctx, key, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return false, bootstrapv1.SSHUnreachableReason, fmt.Sprintf("ssh auth secret %s/%s not found", ns, secretName), nil
		}
		return false, bootstrapv1.SSHProbeErrorReason, "", err
	}
	priv := sec.Data[corev1.SSHAuthPrivateKey]
	if len(priv) == 0 {
		return false, bootstrapv1.SSHProbeErrorReason, "ssh auth secret missing ssh-privatekey", nil
	}
	signer, err := sshlib.ParsePrivateKey(priv)
	if err != nil {
		return false, bootstrapv1.SSHProbeErrorReason, fmt.Sprintf("parse ssh key failed: %v", err), nil
	}
	cfg := &sshlib.ClientConfig{
		User:            "root",
		Auth:            []sshlib.AuthMethod{sshlib.PublicKeys(signer)},
		HostKeyCallback: sshlib.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	// Connect to bastion
	bastionAddr := fmt.Sprintf("%s:22", fip)
	bastionCli, err := sshlib.Dial("tcp", bastionAddr, cfg)
	if err != nil {
		return false, bootstrapv1.SSHUnreachableReason, fmt.Sprintf("connect bastion %s failed: %v", bastionAddr, err), nil
	}
	defer bastionCli.Close()

	// Try to open a TCP tunnel to target:22 via bastion; success means reachable
	targetAddr := fmt.Sprintf("%s:22", targetIP)
	ch, err := bastionCli.Dial("tcp", targetAddr)
	if err != nil {
		return false, bootstrapv1.SSHUnreachableReason, fmt.Sprintf("tunnel to %s failed: %v", targetAddr, err), nil
	}
	_ = ch.Close()

	return true, bootstrapv1.SSHReachableReason, "", nil
}

func setSSHConnectivityCondition(config *bootstrapv1.AnsibleConfig, status metav1.ConditionStatus, reason, message string) {
	conditions.Set(config, metav1.Condition{Type: string(bootstrapv1.SSHConnectivityCondition), Status: status, Reason: reason, Message: message})
}
