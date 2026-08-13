//nolint:testpackage // white-box tests exercise unexported reconciler internals
package azurearm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const testClusterName = "c1"

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

func clusterNameLabels() map[string]string {
	return map[string]string{clusterv1.ClusterNameLabel: testClusterName}
}

func reconcileRequest() ctrl.Request {
	return ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "my-rg"},
	}
}

func TestReconcileSkipsPausedAnnotatedResource(t *testing.T) {
	t.Parallel()

	resourceGroup := newManagedResourceGroup(nil, nil, map[string]string{
		clusterv1.PausedAnnotation: "true",
	})
	reconciler := newDeleteTestReconciler(t, resourceGroup, 0)

	result, err := reconciler.Reconcile(context.Background(), reconcileRequest())

	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result, "paused resource must not requeue")
}

func TestReconcileSkipsResourceOfPausedCluster(t *testing.T) {
	t.Parallel()

	resourceGroup := newManagedResourceGroup(nil, clusterNameLabels(), nil)
	reconciler := newDeleteTestReconciler(t, resourceGroup, 0, newCAPICluster(true))

	result, err := reconciler.Reconcile(context.Background(), reconcileRequest())

	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result, "resource of paused cluster must not requeue")
}

func TestReconcilePausedSkipsDeleteAndRetainsFinalizer(t *testing.T) {
	t.Parallel()

	resourceGroup := newManagedResourceGroup([]string{finalizerName}, clusterNameLabels(), nil)
	now := metav1.Now()
	resourceGroup.DeletionTimestamp = &now

	reconciler := newDeleteTestReconciler(t, resourceGroup, 0, newCAPICluster(true))

	result, err := reconciler.Reconcile(context.Background(), reconcileRequest())

	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	fetched := getResourceGroup(t, reconciler)
	assert.True(t, controllerutil.ContainsFinalizer(fetched, finalizerName),
		"finalizer must be retained while paused so no ARM DELETE is issued")
}

type isPausedCase struct {
	name    string
	labels  map[string]string
	annos   map[string]string
	cluster *clusterv1.Cluster
	want    bool
}

func isPausedCases() []isPausedCase {
	return []isPausedCase{
		{
			name:  "paused annotation on the resource itself",
			annos: map[string]string{clusterv1.PausedAnnotation: "true"},
			want:  true,
		},
		{
			name:    "owning cluster paused",
			labels:  clusterNameLabels(),
			cluster: newCAPICluster(true),
			want:    true,
		},
		{
			name:    "owning cluster not paused",
			labels:  clusterNameLabels(),
			cluster: newCAPICluster(false),
			want:    false,
		},
		{
			name:   "owning cluster missing falls back to own annotation",
			labels: map[string]string{clusterv1.ClusterNameLabel: "gone"},
			annos:  map[string]string{clusterv1.PausedAnnotation: "true"},
			want:   true,
		},
		{
			name:   "owning cluster missing and no annotation",
			labels: map[string]string{clusterv1.ClusterNameLabel: "gone"},
			want:   false,
		},
		{
			name: "no cluster label",
			want: false,
		},
	}
}

func TestIsPaused(t *testing.T) {
	t.Parallel()

	for _, testCase := range isPausedCases() {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			resourceGroup := newManagedResourceGroup(nil, testCase.labels, testCase.annos)

			var extraObjs []client.Object
			if testCase.cluster != nil {
				extraObjs = append(extraObjs, testCase.cluster)
			}

			reconciler := newDeleteTestReconciler(t, resourceGroup, 0, extraObjs...)

			paused, err := reconciler.isPaused(context.Background(), resourceGroup)

			require.NoError(t, err)
			assert.Equal(t, testCase.want, paused)
		})
	}
}
