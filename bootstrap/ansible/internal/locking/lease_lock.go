package locking

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LeaseLock coordinates short-lived critical sections using coordination.k8s.io Lease objects.
type LeaseLock struct {
	client   client.Client
	duration time.Duration
}

// LeaseMetadata captures the identifying properties for a lock.
type LeaseMetadata struct {
	Namespace       string
	Name            string
	Holder          string
	Labels          map[string]string
	OwnerReferences []metav1.OwnerReference
	Logger          logr.Logger
}

// NewLeaseLock instantiates a LeaseLock helper bound to a client and duration.
func NewLeaseLock(c client.Client, duration time.Duration) *LeaseLock {
	return &LeaseLock{client: c, duration: duration}
}

// WithLease acquires the lease described by meta, executes fn, and optionally retains the lease.
// When retain=false the lease is released immediately after fn executes successfully.
func (l *LeaseLock) WithLease(ctx context.Context, meta LeaseMetadata, retain bool, fn func() error) (bool, error) {
	holder := meta.Holder
	if holder == "" {
		return false, errors.New("lease holder identity must be provided")
	}
	for {
		now := metav1.MicroTime{Time: time.Now()}
		lease := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:            meta.Name,
				Namespace:       meta.Namespace,
				Labels:          meta.Labels,
				OwnerReferences: meta.OwnerReferences,
			},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       ptr.To(holder),
				LeaseDurationSeconds: ptr.To(int32(l.duration / time.Second)),
				AcquireTime:          &now,
				RenewTime:            &now,
			},
		}
		if err := l.client.Create(ctx, lease); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return false, errors.Wrapf(err, "failed to acquire lease %s/%s", meta.Namespace, meta.Name)
			}
			existing := &coordinationv1.Lease{}
			if err := l.client.Get(ctx, client.ObjectKey{Namespace: meta.Namespace, Name: meta.Name}, existing); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return false, errors.Wrapf(err, "failed to get lease %s/%s", meta.Namespace, meta.Name)
			}
			if l.expired(existing) {
				if err := l.client.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
					return false, errors.Wrapf(err, "failed to delete stale lease %s/%s", meta.Namespace, meta.Name)
				}
				continue
			}
			existingHolder := ""
			if existing.Spec.HolderIdentity != nil {
				existingHolder = *existing.Spec.HolderIdentity
			}
			if existingHolder != holder {
				return false, nil
			}
			existing.Spec.RenewTime = &metav1.MicroTime{Time: time.Now()}
			if err := l.client.Update(ctx, existing); err != nil {
				if apierrors.IsConflict(err) {
					continue
				}
				return false, errors.Wrapf(err, "failed to update lease %s/%s", meta.Namespace, meta.Name)
			}
			if err := fn(); err != nil {
				return true, err
			}
			if !retain {
				return true, l.Delete(ctx, meta.Namespace, meta.Name)
			}
			return true, nil
		}
		if err := fn(); err != nil {
			if delErr := l.Delete(ctx, meta.Namespace, meta.Name); delErr != nil && meta.Logger.GetSink() != nil {
				meta.Logger.Error(delErr, "failed to release lease after error", "lease", fmt.Sprintf("%s/%s", meta.Namespace, meta.Name))
			}
			return true, err
		}
		if retain {
			return true, nil
		}
		return true, l.Delete(ctx, meta.Namespace, meta.Name)
	}
}

// Delete releases the referenced lease.
func (l *LeaseLock) Delete(ctx context.Context, namespace, name string) error {
	if name == "" {
		return nil
	}
	lease := &coordinationv1.Lease{}
	lease.Namespace = namespace
	lease.Name = name
	if err := l.client.Delete(ctx, lease); err != nil && !apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "failed to delete lease %s/%s", namespace, name)
	}
	return nil
}

func (l *LeaseLock) expired(lease *coordinationv1.Lease) bool {
	var renew time.Time
	switch {
	case lease.Spec.RenewTime != nil:
		renew = lease.Spec.RenewTime.Time
	case lease.Spec.AcquireTime != nil:
		renew = lease.Spec.AcquireTime.Time
	default:
		return true
	}
	duration := l.duration
	if lease.Spec.LeaseDurationSeconds != nil && *lease.Spec.LeaseDurationSeconds > 0 {
		duration = time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second
	}
	return time.Now().After(renew.Add(duration))
}

// GenerateLeaseName truncates the provided base string and appends a deterministic hash.
func GenerateLeaseName(base string) string {
	const suffix = "-lock"
	if len(base)+len(suffix) <= 63 {
		return base + suffix
	}
	baseLen := 63 - len(suffix) - 9
	if baseLen < 1 {
		baseLen = 1
	}
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(base))
	return fmt.Sprintf("%s-%08x%s", base[:baseLen], hasher.Sum32(), suffix)
}
