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

package controllers

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"reflect"

	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/ansible/api/v1alpha1"
	ansiblecloudinit "sigs.k8s.io/cluster-api/bootstrap/ansible/internal/cloudinit"
	"sigs.k8s.io/cluster-api/util"
)

type lockReference struct {
	Namespace string
	Name      string
}

type secretReference struct {
	Name      string
	Namespace string
}

const (
	sshAuthRefKey                = "sshAuthRef"
	sshAuthorizedKeysPath        = "/root/.ssh/authorized_keys"
	sshAuthorizedKeysOwner       = "root:root"
	sshAuthorizedKeysPermissions = "0600"
	sshAuthSecretOwnerKind       = "AnsibleConfig"
)

func (r *AnsibleConfigReconciler) ensureSSHAuthSecretWithLock(ctx context.Context, scope *Scope, ref secretReference, retain bool) (bool, error) {
	ns := sshAuthSecretNamespace(scope, ref)
	lockRef := configMapReference{Name: ref.Name, Namespace: ns}
	acquired, err := r.withLeaseLock(ctx, scope, lockRef, retain, func() error {
		secret, err := r.fetchSSHAuthSecret(ctx, ns, ref.Name)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return errors.Wrapf(err, "failed to get Secret %s/%s", ns, ref.Name)
			}
			privateKey, _, err := generateSSHKeyPair()
			if err != nil {
				return err
			}
			newSecret := buildSSHAuthSecret(scope, ref, ns, privateKey)
			if err := r.createSSHAuthSecret(ctx, newSecret); err != nil {
				if apierrors.IsAlreadyExists(err) {
					return nil
				}
				return errors.Wrapf(err, "failed to create Secret %s/%s", ns, ref.Name)
			}
			return nil
		}

		updated := false
		if ensureSSHAuthSecretMetadata(secret, scope, ns) {
			updated = true
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		if dataUpdated, err := ensureSSHAuthPrivateKey(secret); err != nil {
			return err
		} else if dataUpdated {
			updated = true
		}
		if !updated {
			return nil
		}
		if err := r.updateSSHAuthSecret(ctx, secret); err != nil {
			return errors.Wrapf(err, "failed to update Secret %s/%s", ns, ref.Name)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if !acquired {
		return false, nil
	}
	return true, nil
}

func (r *AnsibleConfigReconciler) buildSSHAuthorizedKeyFile(ctx context.Context, scope *Scope) (*ansiblecloudinit.File, error) {
	ref, err := sshAuthSecretRefFromTemplate(scope)
	if err != nil {
		return nil, err
	}
	if ref.Name == "" {
		return nil, nil
	}
	ready, err := r.ensureSSHAuthSecretWithLock(ctx, scope, ref, false)
	if err != nil {
		return nil, err
	}
	if !ready {
		return nil, nil
	}

	secret, err := r.fetchSSHAuthSecret(ctx, ref.Namespace, ref.Name)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get SSH auth Secret %s/%s", ref.Namespace, ref.Name)
	}
	return sshAuthorizedKeyFileFromSecret(secret, ref.Namespace, ref.Name)
}

func (r *AnsibleConfigReconciler) fetchSSHAuthSecret(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := r.Client.Get(ctx, key, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

func (r *AnsibleConfigReconciler) createSSHAuthSecret(ctx context.Context, secret *corev1.Secret) error {
	if err := r.Client.Create(ctx, secret); err != nil {
		return err
	}
	return nil
}

func (r *AnsibleConfigReconciler) updateSSHAuthSecret(ctx context.Context, secret *corev1.Secret) error {
	if err := r.Client.Update(ctx, secret); err != nil {
		return err
	}
	return nil
}

func sshAuthSecretRefFromTemplate(scope *Scope) (secretReference, error) {
	if scope.ClusterTemplate == nil {
		return secretReference{}, nil
	}
	specMap, err := decodeTemplateSpec(scope.ClusterTemplate.Spec)
	if err != nil {
		return secretReference{}, err
	}
	defaultNamespace := scope.ClusterTemplate.Namespace
	if defaultNamespace == "" {
		defaultNamespace = scope.Config.Namespace
	}
	return parseNamespacedSecretRef(specMap, sshAuthRefKey, defaultNamespace), nil
}

func sshAuthSecretNamespace(scope *Scope, ref secretReference) string {
	if ref.Namespace != "" {
		return ref.Namespace
	}
	return scope.Config.Namespace
}

func buildSSHAuthSecret(scope *Scope, ref secretReference, namespace string, privateKey []byte) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ref.Name,
			Namespace: namespace,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: scope.Cluster.Name,
			},
		},
		Type: corev1.SecretTypeSSHAuth,
		Data: map[string][]byte{
			corev1.SSHAuthPrivateKey: privateKey,
		},
	}
	if namespace == scope.Config.Namespace {
		secret.SetOwnerReferences(util.EnsureOwnerRef(nil, metav1.OwnerReference{
			APIVersion: bootstrapv1.GroupVersion.String(),
			Kind:       sshAuthSecretOwnerKind,
			Name:       scope.Config.Name,
			UID:        scope.Config.UID,
			Controller: ptr.To(true),
		}))
	}
	return secret
}

func ensureSSHAuthSecretMetadata(secret *corev1.Secret, scope *Scope, namespace string) bool {
	updated := false
	if secret.Labels == nil {
		secret.Labels = map[string]string{}
	}
	if secret.Labels[clusterv1.ClusterNameLabel] != scope.Cluster.Name {
		secret.Labels[clusterv1.ClusterNameLabel] = scope.Cluster.Name
		updated = true
	}
	if secret.Type != corev1.SecretTypeSSHAuth {
		secret.Type = corev1.SecretTypeSSHAuth
		updated = true
	}
	if namespace == scope.Config.Namespace {
		ownerRef := metav1.OwnerReference{
			APIVersion: bootstrapv1.GroupVersion.String(),
			Kind:       sshAuthSecretOwnerKind,
			Name:       scope.Config.Name,
			UID:        scope.Config.UID,
			Controller: ptr.To(true),
		}
		ownerRefs := util.EnsureOwnerRef(secret.GetOwnerReferences(), ownerRef)
		if !reflect.DeepEqual(ownerRefs, secret.GetOwnerReferences()) {
			secret.SetOwnerReferences(ownerRefs)
			updated = true
		}
	}
	return updated
}

func ensureSSHAuthPrivateKey(secret *corev1.Secret) (bool, error) {
	privateKey, hasPrivate := secret.Data[corev1.SSHAuthPrivateKey]
	if hasPrivate && len(privateKey) > 0 {
		return false, nil
	}
	privateKey, _, err := generateSSHKeyPair()
	if err != nil {
		return false, err
	}
	secret.Data[corev1.SSHAuthPrivateKey] = privateKey
	return true, nil
}

func sshAuthorizedKeyFileFromSecret(secret *corev1.Secret, namespace, name string) (*ansiblecloudinit.File, error) {
	privateKey, err := sshAuthPrivateKeyFromSecret(secret, namespace, name)
	if err != nil {
		return nil, err
	}
	publicKey, err := deriveSSHPublicKey(privateKey)
	if err != nil {
		return nil, err
	}
	return buildAuthorizedKeysFile(publicKey), nil
}

func sshAuthPrivateKeyFromSecret(secret *corev1.Secret, namespace, name string) ([]byte, error) {
	privateKey, hasPrivate := secret.Data[corev1.SSHAuthPrivateKey]
	if !hasPrivate || len(privateKey) == 0 {
		return nil, errors.Errorf("SSH auth Secret %s/%s is missing ssh-privatekey", namespace, name)
	}
	return privateKey, nil
}

func buildAuthorizedKeysFile(publicKey []byte) *ansiblecloudinit.File {
	content := string(publicKey)
	if len(content) > 0 && content[len(content)-1] != '\n' {
		content += "\n"
	}
	return &ansiblecloudinit.File{
		Path:        sshAuthorizedKeysPath,
		Owner:       sshAuthorizedKeysOwner,
		Permissions: sshAuthorizedKeysPermissions,
		Append:      true,
		Content:     content,
	}
}

func generateSSHKeyPair() ([]byte, []byte, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to generate SSH private key")
	}
	privateKeyBytes := x509.MarshalPKCS1PrivateKey(privateKey)
	privateKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: privateKeyBytes})
	publicKey, err := ssh.NewPublicKey(&privateKey.PublicKey)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to derive SSH public key")
	}
	publicKeyBytes := ssh.MarshalAuthorizedKey(publicKey)
	return privateKeyPEM, publicKeyBytes, nil
}

func deriveSSHPublicKey(privateKeyPEM []byte) ([]byte, error) {
	signer, err := ssh.ParsePrivateKey(privateKeyPEM)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse SSH private key")
	}
	return ssh.MarshalAuthorizedKey(signer.PublicKey()), nil
}
