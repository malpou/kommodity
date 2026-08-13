//nolint:testpackage // white-box tests exercise unexported reconciler internals
package azurearm

import (
	"context"
	"testing"

	resourcesv1 "github.com/Azure/azure-service-operator/v2/api/resources/v1api20200601"
	"github.com/Azure/azure-service-operator/v2/pkg/genruntime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const testClusterName = "c1"

// newPauseTestScheme extends the ASO test scheme with the CAPI Cluster types so
// pause resolution via the cluster.x-k8s.io/cluster-name label can be exercised.
func newPauseTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := newTestScheme(t)

	err := clusterv1.AddToScheme(scheme)
	require.NoError(t, err, "adding cluster-api scheme")

	return scheme
}

func newPauseTestReconciler(t *testing.T, objs ...client.Object) *Reconciler {
	t.Helper()

	kubeClient := fake.NewClientBuilder().
		WithScheme(newPauseTestScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&resourcesv1.ResourceGroup{}).
		Build()

	return &Reconciler{
		Client:         kubeClient,
		controllerName: "azurearm-resourcegroup",
		newObj:         func() genruntime.ARMMetaObject { return &resourcesv1.ResourceGroup{} },
		armIDFor:       resourceGroupARMID,
	}
}

func newCAPICluster(paused bool) *clusterv1.Cluster {
	return &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNamespace,
			Name:      testClusterName,
		},
		Spec: clusterv1.ClusterSpec{
			Paused: paused,
		},
	}
}

func newLabeledResourceGroup(labels map[string]string, annos map[string]string) *resourcesv1.ResourceGroup {
	resourceGroup := newResourceGroup("my-rg")
	resourceGroup.ObjectMeta = metav1.ObjectMeta{
		Namespace:   testNamespace,
		Name:        "my-rg",
		Labels:      labels,
		Annotations: annos,
	}

	return resourceGroup
}

func reconcileRequest() ctrl.Request {
	return ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "my-rg"},
	}
}

func TestReconcileSkipsPausedAnnotatedResource(t *testing.T) {
	t.Parallel()

	resourceGroup := newLabeledResourceGroup(nil, map[string]string{
		clusterv1.PausedAnnotation: "true",
	})
	reconciler := newPauseTestReconciler(t, resourceGroup)

	result, err := reconciler.Reconcile(context.Background(), reconcileRequest())

	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result, "paused resource must not requeue")
}

func TestReconcileSkipsResourceOfPausedCluster(t *testing.T) {
	t.Parallel()

	resourceGroup := newLabeledResourceGroup(map[string]string{
		clusterv1.ClusterNameLabel: testClusterName,
	}, nil)
	reconciler := newPauseTestReconciler(t, resourceGroup, newCAPICluster(true))

	result, err := reconciler.Reconcile(context.Background(), reconcileRequest())

	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result, "resource of paused cluster must not requeue")
}

func TestReconcilePausedSkipsDeleteAndRetainsFinalizer(t *testing.T) {
	t.Parallel()

	resourceGroup := newLabeledResourceGroup(map[string]string{
		clusterv1.ClusterNameLabel: testClusterName,
	}, nil)
	resourceGroup.Finalizers = []string{finalizerName}
	now := metav1.Now()
	resourceGroup.DeletionTimestamp = &now

	reconciler := newPauseTestReconciler(t, resourceGroup, newCAPICluster(true))

	result, err := reconciler.Reconcile(context.Background(), reconcileRequest())

	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	fetched := &resourcesv1.ResourceGroup{}
	err = reconciler.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: "my-rg"}, fetched)
	require.NoError(t, err)
	assert.True(t, controllerutil.ContainsFinalizer(fetched, finalizerName),
		"finalizer must be retained while paused so no ARM DELETE is issued")
}

func TestIsPaused(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		labels  map[string]string
		annos   map[string]string
		cluster *clusterv1.Cluster
		want    bool
	}{
		{
			name:  "paused annotation on the resource itself",
			annos: map[string]string{clusterv1.PausedAnnotation: "true"},
			want:  true,
		},
		{
			name:    "owning cluster paused",
			labels:  map[string]string{clusterv1.ClusterNameLabel: testClusterName},
			cluster: newCAPICluster(true),
			want:    true,
		},
		{
			name:    "owning cluster not paused",
			labels:  map[string]string{clusterv1.ClusterNameLabel: testClusterName},
			cluster: newCAPICluster(false),
			want:    false,
		},
		{
			name:   "owning cluster missing",
			labels: map[string]string{clusterv1.ClusterNameLabel: "gone"},
			want:   false,
		},
		{
			name: "no cluster label",
			want: false,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			resourceGroup := newLabeledResourceGroup(testCase.labels, testCase.annos)

			objs := []client.Object{resourceGroup}
			if testCase.cluster != nil {
				objs = append(objs, testCase.cluster)
			}

			reconciler := newPauseTestReconciler(t, objs...)

			paused, err := reconciler.isPaused(context.Background(), resourceGroup)

			require.NoError(t, err)
			assert.Equal(t, testCase.want, paused)
		})
	}
}
