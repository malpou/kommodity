package config

const (
	// KommodityNamespace is the namespace where Kommodity components operate.
	KommodityNamespace = "kommodity-system"
	// ManagedByLabel is the label key used to indicate resources managed by Kommodity.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	// DeploymentNameLabel is the label key used to indicate the deployment name.
	DeploymentNameLabel = "cluster.x-k8s.io/deployment-name"
	// SelfHostedAnnotation marks a CAPI Cluster as hosting the Kommodity management
	// plane itself. Deletion of a Cluster carrying this annotation is blocked by the
	// self-hosted cluster admission webhook until a reverse pivot (state handover)
	// has moved the management plane off the cluster.
	SelfHostedAnnotation = "kommodity.io/self-hosted"
	// AllowSelfHostedDeleteAnnotation overrides the self-hosted deletion guardrail
	// for a deliberate teardown. Setting it is an explicit, auditable act; the
	// future reverse-pivot flow sets it only after verifying the management plane
	// no longer runs on the cluster.
	AllowSelfHostedDeleteAnnotation = "kommodity.io/allow-self-hosted-delete"
)

// GetKommodityLabels returns the standard labels for Kommodity-managed resources.
func GetKommodityLabels(nodeUUID, nodeIP string) map[string]string {
	return map[string]string{
		ManagedByLabel:        "kommodity",
		"talos.dev/node-uuid": nodeUUID,
		"talos.dev/node-ip":   nodeIP,
	}
}
