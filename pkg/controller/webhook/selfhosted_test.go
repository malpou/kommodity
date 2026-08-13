package webhook_test

import (
	"context"
	"testing"

	"github.com/kommodity-io/kommodity/pkg/config"
	"github.com/kommodity-io/kommodity/pkg/controller/webhook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

const (
	testClusterNamespace = "fleet"
	testClusterName      = "management"
	annotationEnabled    = "true"
)

func newCluster(annotations map[string]string) *clusterv1.Cluster {
	return &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   testClusterNamespace,
			Name:        testClusterName,
			Annotations: annotations,
		},
	}
}

func TestValidateDeleteBlocksAnnotatedSelfHostedCluster(t *testing.T) {
	t.Parallel()

	validator := webhook.NewSelfHostedClusterValidator("")
	cluster := newCluster(map[string]string{config.SelfHostedAnnotation: annotationEnabled})

	warnings, err := validator.ValidateDelete(context.Background(), cluster)

	require.ErrorIs(t, err, webhook.ErrSelfHostedClusterDeletionBlocked)
	assert.Empty(t, warnings)
}

func TestValidateDeleteBlocksEnvConfiguredSelfHostedCluster(t *testing.T) {
	t.Parallel()

	validator := webhook.NewSelfHostedClusterValidator(testClusterNamespace + "/" + testClusterName)
	cluster := newCluster(nil)

	warnings, err := validator.ValidateDelete(context.Background(), cluster)

	require.ErrorIs(t, err, webhook.ErrSelfHostedClusterDeletionBlocked)
	assert.Empty(t, warnings)
}

func TestValidateDeleteAllowsOverrideAnnotation(t *testing.T) {
	t.Parallel()

	validator := webhook.NewSelfHostedClusterValidator(testClusterNamespace + "/" + testClusterName)
	cluster := newCluster(map[string]string{
		config.SelfHostedAnnotation:            annotationEnabled,
		config.AllowSelfHostedDeleteAnnotation: annotationEnabled,
	})

	warnings, err := validator.ValidateDelete(context.Background(), cluster)

	require.NoError(t, err)
	assert.Len(t, warnings, 1, "override deletion should surface a warning")
}

func TestValidateDeleteAllowsUnmarkedCluster(t *testing.T) {
	t.Parallel()

	validator := webhook.NewSelfHostedClusterValidator("")
	cluster := newCluster(nil)

	warnings, err := validator.ValidateDelete(context.Background(), cluster)

	require.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestValidateDeleteAllowsDifferentEnvConfiguredCluster(t *testing.T) {
	t.Parallel()

	validator := webhook.NewSelfHostedClusterValidator("other-ns/other-cluster")
	cluster := newCluster(nil)

	warnings, err := validator.ValidateDelete(context.Background(), cluster)

	require.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestValidateDeleteRejectsUnexpectedObjectType(t *testing.T) {
	t.Parallel()

	validator := webhook.NewSelfHostedClusterValidator("")

	warnings, err := validator.ValidateDelete(context.Background(), &clusterv1.Machine{})

	require.ErrorIs(t, err, webhook.ErrUnexpectedObjectType)
	assert.Empty(t, warnings)
}

func TestValidateUpdateBlocksMarkerRemoval(t *testing.T) {
	t.Parallel()

	validator := webhook.NewSelfHostedClusterValidator("")
	oldCluster := newCluster(map[string]string{config.SelfHostedAnnotation: annotationEnabled})
	newCluster := newCluster(nil)

	warnings, err := validator.ValidateUpdate(context.Background(), oldCluster, newCluster)

	require.ErrorIs(t, err, webhook.ErrSelfHostedMarkerRemovalBlocked)
	assert.Empty(t, warnings)
}

func TestValidateUpdateAllowsMarkerRemovalWithOverride(t *testing.T) {
	t.Parallel()

	validator := webhook.NewSelfHostedClusterValidator("")
	oldCluster := newCluster(map[string]string{config.SelfHostedAnnotation: annotationEnabled})
	newCluster := newCluster(map[string]string{
		config.AllowSelfHostedDeleteAnnotation: annotationEnabled,
	})

	warnings, err := validator.ValidateUpdate(context.Background(), oldCluster, newCluster)

	require.NoError(t, err)
	assert.Len(t, warnings, 1, "override removal should surface a warning")
}

func TestValidateUpdateAllowsUnrelatedChanges(t *testing.T) {
	t.Parallel()

	validator := webhook.NewSelfHostedClusterValidator("")
	marked := newCluster(map[string]string{config.SelfHostedAnnotation: annotationEnabled})
	unmarked := newCluster(nil)

	warnings, err := validator.ValidateUpdate(context.Background(), marked, marked)
	require.NoError(t, err)
	assert.Empty(t, warnings)

	// Adding the marker is always allowed.
	warnings, err = validator.ValidateUpdate(context.Background(), unmarked, marked)
	require.NoError(t, err)
	assert.Empty(t, warnings)

	// Updates to clusters that never carried the marker are always allowed.
	warnings, err = validator.ValidateUpdate(context.Background(), unmarked, unmarked)
	require.NoError(t, err)
	assert.Empty(t, warnings)
}
