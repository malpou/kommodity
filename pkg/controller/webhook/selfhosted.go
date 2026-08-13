package webhook

import (
	"context"
	"fmt"

	"github.com/kommodity-io/kommodity/pkg/config"
	"github.com/kommodity-io/kommodity/pkg/logging"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	// selfHostedClusterValidationPath is the HTTP path the self-hosted cluster
	// validation webhook is served on. It must stay in sync with the path declared
	// in pkg/provider/kommodity/kommodity-validating-webhook-configuration.yaml.
	selfHostedClusterValidationPath = "/validate-kommodity-io-self-hosted-cluster"

	// annotationEnabledValue is the value that activates a marker annotation.
	annotationEnabledValue = "true"
)

// SelfHostedClusterValidator blocks deletion of the CAPI Cluster hosting the
// Kommodity management plane itself (FR17 of the state-handover PRD). A
// self-managed Kommodity that deletes its own hosting cluster would destroy its
// own control plane mid-deletion and orphan infrastructure; deletion must go
// through a reverse pivot to an external instance instead.
type SelfHostedClusterValidator struct {
	// selfHostedCluster is the "<namespace>/<name>" of the hosting Cluster from
	// KOMMODITY_SELF_HOSTED_CLUSTER; empty disables the environment-based marker.
	selfHostedCluster string
}

// NewSelfHostedClusterValidator builds a validator for the self-hosted Cluster
// deletion guardrail. selfHostedCluster is the "<namespace>/<name>" of the
// hosting Cluster (from KOMMODITY_SELF_HOSTED_CLUSTER); empty disables the
// environment-based marker, leaving only the annotation-based one.
func NewSelfHostedClusterValidator(selfHostedCluster string) *SelfHostedClusterValidator {
	return &SelfHostedClusterValidator{
		selfHostedCluster: selfHostedCluster,
	}
}

// setupSelfHostedClusterWebhook registers the self-hosted Cluster deletion
// guardrail on the manager's webhook server.
func setupSelfHostedClusterWebhook(manager ctrl.Manager, selfHostedCluster string) {
	validator := NewSelfHostedClusterValidator(selfHostedCluster)

	manager.GetWebhookServer().Register(
		selfHostedClusterValidationPath,
		admission.WithCustomValidator(manager.GetScheme(), &clusterv1.Cluster{}, validator),
	)
}

// ValidateCreate implements admission.CustomValidator; creation is always allowed.
func (v *SelfHostedClusterValidator) ValidateCreate(
	_ context.Context,
	_ runtime.Object,
) (admission.Warnings, error) {
	return nil, nil
}

// ValidateUpdate implements admission.CustomValidator; updates are always allowed.
func (v *SelfHostedClusterValidator) ValidateUpdate(
	_ context.Context,
	_ runtime.Object,
	_ runtime.Object,
) (admission.Warnings, error) {
	return nil, nil
}

// ValidateDelete blocks the request when the Cluster is marked as self-hosted,
// either via the kommodity.io/self-hosted annotation or via the
// KOMMODITY_SELF_HOSTED_CLUSTER environment configuration, unless the
// kommodity.io/allow-self-hosted-delete override annotation is present.
func (v *SelfHostedClusterValidator) ValidateDelete(
	ctx context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	logger := logging.FromContext(ctx)

	cluster, success := obj.(*clusterv1.Cluster)
	if !success {
		return nil, fmt.Errorf("%w: expected Cluster, got %T", ErrUnexpectedObjectType, obj)
	}

	if !v.isSelfHosted(cluster) {
		return nil, nil
	}

	annotations := cluster.GetAnnotations()
	if annotations[config.AllowSelfHostedDeleteAnnotation] == annotationEnabledValue {
		logger.Warn("Allowing deletion of self-hosted cluster due to override annotation",
			zap.String("cluster", cluster.GetNamespace()+"/"+cluster.GetName()),
			zap.String("annotation", config.AllowSelfHostedDeleteAnnotation))

		warning := fmt.Sprintf("deleting self-hosted cluster %s/%s: the %s override annotation is set",
			cluster.GetNamespace(), cluster.GetName(), config.AllowSelfHostedDeleteAnnotation)

		return admission.Warnings{warning}, nil
	}

	logger.Warn("Blocking deletion of self-hosted cluster",
		zap.String("cluster", cluster.GetNamespace()+"/"+cluster.GetName()))

	return nil, fmt.Errorf("%w: cluster %s/%s",
		ErrSelfHostedClusterDeletionBlocked, cluster.GetNamespace(), cluster.GetName())
}

// isSelfHosted reports whether the Cluster carries the self-hosted annotation or
// matches the environment-configured "<namespace>/<name>" marker.
func (v *SelfHostedClusterValidator) isSelfHosted(cluster *clusterv1.Cluster) bool {
	if cluster.GetAnnotations()[config.SelfHostedAnnotation] == annotationEnabledValue {
		return true
	}

	if v.selfHostedCluster == "" {
		return false
	}

	return v.selfHostedCluster == cluster.GetNamespace()+"/"+cluster.GetName()
}
