package workloadprobe

import (
	"context"
	"net/http"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	remote "sigs.k8s.io/cluster-api/controllers/remote"
)

// WorkloadProbe defines an interface to determine if a workload cluster control plane is initialized.
// Implementations should be idempotent and fast; callers may invoke this on every reconcile.
type WorkloadProbe interface {
	// IsInitialized returns true if the workload cluster can be considered initialized/available.
	IsInitialized(ctx context.Context, c client.Reader, cluster *clusterv1.Cluster) (bool, error)
}

// livezProbe probes the API server /livez endpoint using the cluster's kubeconfig.
type livezProbe struct {
	timeout time.Duration
}

// NewLivezProbe creates a probe that considers the control plane initialized when /livez returns HTTP 200.
func NewLivezProbe(timeout time.Duration) WorkloadProbe { return &livezProbe{timeout: timeout} }

func (p *livezProbe) IsInitialized(ctx context.Context, c client.Reader, cluster *clusterv1.Cluster) (bool, error) {
	if cluster == nil {
		return false, nil
	}
	// Build REST config from <cluster>-kubeconfig Secret via CAPI helper.
	restCfg, err := remote.RESTConfig(ctx, "acp", c, client.ObjectKey{Namespace: cluster.Namespace, Name: cluster.Name})
	if err != nil {
		return false, err
	}
	if restCfg == nil || restCfg.Host == "" {
		return false, nil
	}

	// Compose /livez URL.
	u, err := url.Parse(restCfg.Host)
	if err != nil {
		return false, err
	}
	u.Path = "/livez"

	// Build HTTP client honoring kubeconfig TLS.
	rt, err := rest.TransportFor(restCfg)
	if err != nil {
		return false, err
	}
	httpClient := &http.Client{Transport: rt, Timeout: p.effectiveTimeout()}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = drainAndClose(resp) }()
	return resp.StatusCode == http.StatusOK, nil
}

func (p *livezProbe) effectiveTimeout() time.Duration {
	if p.timeout <= 0 {
		return 3 * time.Second
	}
	return p.timeout
}

// drainAndClose ensures body is closed and small bodies are drained for connection reuse.
func drainAndClose(resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return nil
	}
	// Best-effort: read and discard up to a small buffer.
	buf := make([]byte, 512)
	_, _ = resp.Body.Read(buf)
	return resp.Body.Close()
}

// _ ensure no unused imports when building in different environments
var _ corev1.ConditionStatus
