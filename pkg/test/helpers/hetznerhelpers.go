package helpers

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	k8s_wait "k8s.io/apimachinery/pkg/util/wait"
)

const (
	hetznerValuesFile = "values.hetzner.yaml"
	hetznerTestSKU    = "cpx22"

	// hetznerTestControlPlanes and hetznerTestWorkers pin the cluster size the
	// integration test creates; HetznerTestServerCount asserts on their sum.
	hetznerTestControlPlanes = 3
	hetznerTestWorkers       = 2

	// HetznerTestServerCount is the number of hcloud servers the integration
	// test expects once the cluster is up.
	HetznerTestServerCount = hetznerTestControlPlanes + hetznerTestWorkers
)

// HetznerInfra holds Hetzner-specific configuration for chart installation.
type HetznerInfra struct{}

// ValuesFile returns the Helm values file for Hetzner.
func (h HetznerInfra) ValuesFile() string { return hetznerValuesFile }

// Overrides returns the Helm value overrides for Hetzner testing. The replica
// counts are pinned here so the test owns the server count it asserts on,
// independent of the example values file.
func (h HetznerInfra) Overrides() map[string]any {
	return map[string]any{
		"kommodity.nodepools.default.sku": hetznerTestSKU,
		"kommodity.controlplane.sku":      hetznerTestSKU,
		// int64: unstructured.SetNestedField rejects plain int values.
		"kommodity.controlplane.replicas":      int64(hetznerTestControlPlanes),
		"kommodity.nodepools.default.replicas": int64(hetznerTestWorkers),
	}
}

// WaitForHetznerServers waits until the expected number of hcloud servers exist for the cluster.
func WaitForHetznerServers(
	ctx context.Context,
	clusterName string,
	expectedCount int,
	timeout time.Duration,
) error {
	client, err := getHetznerClient()
	if err != nil {
		return fmt.Errorf("failed to get Hetzner client: %w", err)
	}

	err = k8s_wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true,
		func(pollCtx context.Context) (bool, error) {
			servers, err := listHetznerClusterServers(pollCtx, client, clusterName)
			if err != nil {
				return false, err
			}

			switch {
			case len(servers) < expectedCount:
				log.Printf("Found %d servers in Hetzner. Waiting for %d", len(servers), expectedCount)

				return false, nil
			case len(servers) == expectedCount:
				log.Printf("Found %d servers in Hetzner", len(servers))

				return true, nil
			default:
				return false, fmt.Errorf("%w in Hetzner: found %d, expected %d",
					errMoreServersThanExpected, len(servers), expectedCount)
			}
		})
	if err != nil {
		return fmt.Errorf("%d servers not found in Hetzner within timeout: %w", expectedCount, err)
	}

	return nil
}

// WaitForHetznerServersDeletion waits until all hcloud servers of the cluster are deleted.
func WaitForHetznerServersDeletion(
	ctx context.Context,
	clusterName string,
	timeout time.Duration,
) error {
	client, err := getHetznerClient()
	if err != nil {
		return fmt.Errorf("failed to get Hetzner client: %w", err)
	}

	err = k8s_wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true,
		func(pollCtx context.Context) (bool, error) {
			servers, err := listHetznerClusterServers(pollCtx, client, clusterName)
			if err != nil {
				return false, err
			}

			if len(servers) == 0 {
				log.Printf("All Hetzner servers have been deleted")

				return true, nil
			}

			log.Printf("There are still %d Hetzner servers present", len(servers))

			return false, nil
		})
	if err != nil {
		return fmt.Errorf("hetzner servers were not deleted within the timeout: %w", err)
	}

	return nil
}

// DeleteAllHetznerServers deletes all servers in the Hetzner project of the configured token.
func DeleteAllHetznerServers(ctx context.Context) error {
	client, err := getHetznerClient()
	if err != nil {
		return fmt.Errorf("failed to get Hetzner client: %w", err)
	}

	servers, err := client.Server.All(ctx)
	if err != nil {
		return fmt.Errorf("failed to list Hetzner servers: %w", err)
	}

	for _, server := range servers {
		_, _, err := client.Server.DeleteWithResult(ctx, server)
		if err != nil {
			return fmt.Errorf("failed to delete Hetzner server %s: %w", server.Name, err)
		}

		log.Printf("Deleted Hetzner server %s", server.Name)
	}

	return nil
}

// listHetznerClusterServers lists the hcloud servers owned by the given CAPH cluster.
func listHetznerClusterServers(
	ctx context.Context,
	client *hcloud.Client,
	clusterName string,
) ([]*hcloud.Server, error) {
	servers, err := client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{
			// CAPH labels every server it owns with caph-cluster-<clustername>=owned.
			LabelSelector: "caph-cluster-" + clusterName + "=owned",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list Hetzner servers: %w", err)
	}

	return servers, nil
}

func getHetznerClient() (*hcloud.Client, error) {
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		return nil, errHcloudTokenNotSet
	}

	return hcloud.NewClient(hcloud.WithToken(token)), nil
}
