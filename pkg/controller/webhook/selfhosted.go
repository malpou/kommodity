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
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// ValidateUpdate blocks removal of the self-hosted marker annotation unless the
// override annotation is present, so the deletion guardrail cannot be silently
// stripped in one update before deleting. Everything else is allowed.
func (v *SelfHostedClusterValidator) ValidateUpdate(
	ctx context.Context,
	oldObj runtime.Object,
	newObj runtime.Object,
) (admission.Warnings, error) {
	logger := logging.FromContext(ctx)

	oldCluster, err := toCluster(oldObj)
	if err != nil {
		return nil, err
	}

	newCluster, err := toCluster(newObj)
	if err != nil {
		return nil, err
	}

	_, oldMarked := oldCluster.GetAnnotations()[config.SelfHostedAnnotation]
	_, newMarked := newCluster.GetAnnotations()[config.SelfHostedAnnotation]

	if !oldMarked || newMarked {
		return nil, nil
	}

	key := client.ObjectKeyFromObject(newCluster).String()

	if newCluster.GetAnnotations()[config.AllowSelfHostedDeleteAnnotation] == annotationEnabledValue {
		logger.Warn("Allowing removal of self-hosted marker due to override annotation",
			zap.String("cluster", key),
			zap.String("annotation", config.AllowSelfHostedDeleteAnnotation))

		warning := fmt.Sprintf("removing the %s marker from cluster %s: the %s override annotation is set",
			config.SelfHostedAnnotation, key, config.AllowSelfHostedDeleteAnnotation)

		return admission.Warnings{warning}, nil
	}

	logger.Warn("Blocking removal of self-hosted marker", zap.String("cluster", key))

	return nil, fmt.Errorf("%w: cluster %s", ErrSelfHostedMarkerRemovalBlocked, key)
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

	cluster, err := toCluster(obj)
	if err != nil {
		return nil, err
	}

	key := client.ObjectKeyFromObject(cluster).String()

	if !v.isSelfHosted(cluster, key) {
		return nil, nil
	}

	if cluster.GetAnnotations()[config.AllowSelfHostedDeleteAnnotation] == annotationEnabledValue {
		logger.Warn("Allowing deletion of self-hosted cluster due to override annotation",
			zap.String("cluster", key),
			zap.String("annotation", config.AllowSelfHostedDeleteAnnotation))

		warning := fmt.Sprintf("deleting self-hosted cluster %s: the %s override annotation is set",
			key, config.AllowSelfHostedDeleteAnnotation)

		return admission.Warnings{warning}, nil
	}

	logger.Warn("Blocking deletion of self-hosted cluster", zap.String("cluster", key))

	return nil, fmt.Errorf("%w: cluster %s", ErrSelfHostedClusterDeletionBlocked, key)
}

// toCluster asserts that an admission object is a CAPI Cluster.
func toCluster(obj runtime.Object) (*clusterv1.Cluster, error) {
	cluster, success := obj.(*clusterv1.Cluster)
	if !success {
		return nil, fmt.Errorf("%w: expected Cluster, got %T", ErrUnexpectedObjectType, obj)
	}

	return cluster, nil
}

// isSelfHosted reports whether the Cluster carries the self-hosted annotation or
// matches the environment-configured "<namespace>/<name>" marker. Like CAPI's
// paused annotation, the marker is presence-based so the guardrail fails closed
// on value typos ("True", "yes", ...); remove the annotation (with the override
// set) rather than changing its value.
func (v *SelfHostedClusterValidator) isSelfHosted(cluster *clusterv1.Cluster, key string) bool {
	_, marked := cluster.GetAnnotations()[config.SelfHostedAnnotation]
	if marked {
		return true
	}

	return v.selfHostedCluster != "" && v.selfHostedCluster == key
}
