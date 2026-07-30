# Running Kommodity Clusters on Hetzner Cloud

This guide covers everything needed to deploy Talos-based workload clusters on
Hetzner Cloud (hcloud) through Kommodity, using the
[Syself cluster-api-provider-hetzner](https://github.com/syself/cluster-api-provider-hetzner)
(CAPH) infrastructure provider. Bare-metal (Hetzner Robot) machines are not
supported yet - cloud servers only.

## 1. Prerequisites

- A Hetzner Cloud **project**. Use one project per set of related clusters:
  the API rate limit (see §7) and the API token are both project-scoped.
- A **read/write API token** for that project
  (Hetzner Console → project → Security → API tokens).
- A Talos **snapshot** in the project (see §2).
- Optional: an SSH key uploaded to the project. Talos has no SSH, so this is
  normally not needed.

## 2. Talos node image (snapshot)

Hetzner Cloud does not support direct custom-image upload; Talos images are
delivered as **snapshots**. The
[`talos-cloud-image` workflow](../.github/workflows/talos-cloud-image.yml)
builds a Talos `hcloud`-platform raw image (with the Kommodity attestation and
auto-bootstrap extensions baked in) for the `hcloud` platform. Upload it with
[hcloud-upload-image](https://github.com/apricote/hcloud-upload-image):

```bash
hcloud-upload-image upload \
  --image-path kommodity-talos-hcloud-v1.13.0.raw \
  --architecture x86 \
  --description kommodity-talos-hcloud-v1.13.0 \
  --labels caph-image-name=kommodity-talos-hcloud-v1.13.0
```

The `caph-image-name` label is mandatory: snapshots have no name, so CAPH
resolves `HCloudMachineTemplate.spec.template.spec.imageName` via that label
(with an architecture filter derived from the server type). ARM (CAX) server
types need an arm64 snapshot (`--architecture arm`); keep amd64 and arm64
snapshots as separate uploads with distinct image names.

Alternatively, plain Talos factory images work too:
`--image-url https://factory.talos.dev/image/<schematic>/v1.13.0/hcloud-amd64.raw.xz --compression xz`.

For programmatic uploads (CI, test harnesses), the same project ships a Go
library, `github.com/apricote/hcloud-upload-image/hcloudimages/v2`: a single
`Client.Upload` call performs the whole temporary-server/rescue/snapshot dance,
and `Client.CleanupTemporaryResources` removes leftovers from failed runs.
The upload briefly runs a temporary server, which is billed by Hetzner.

## 3. Enabling the provider

Hetzner is enabled by default. To restrict Kommodity to specific providers:

```bash
KOMMODITY_INFRASTRUCTURE_PROVIDERS=hetzner
```

## 4. Per-cluster setup

Create the API token Secret in the cluster's namespace (CAPH convention - key
must be `hcloud`):

```bash
kubectl --kubeconfig kommodity.yaml create secret generic hetzner \
  --from-literal=hcloud=$HCLOUD_TOKEN
```

Then deploy with the chart:

```bash
helm template my-cluster charts/kommodity-cluster \
  -f charts/kommodity-cluster/values.hetzner.yaml \
  | kubectl --kubeconfig kommodity.yaml apply -f -
```

See [`values.hetzner.yaml`](../charts/kommodity-cluster/values.hetzner.yaml)
for the full set of options: location (`region`: fsn1, nbg1, hel1, ash, hil,
sin), server types (`sku`), replicas, load balancer type, and the Talos image
name. Root disk size is fixed by the server type - there is no disk-size knob.

## 5. What the chart does

- `HetznerCluster` with a control-plane **load balancer** (hcloud LB, type
  `lb11` by default) in the chosen location. For single-control-plane setups
  the LB can be disabled and `controlPlaneEndpoint` pointed at a DNS name.
- `HCloudMachineTemplate` per control plane and node pool.
- **CCM**: `hcloud-cloud-controller-manager` is delivered via
  ClusterResourceSet; the token Secret is delivered to the workload cluster's
  `kube-system` as `hcloud` (CAPH's own secret replication is disabled to keep
  a single writer). Services of type LoadBalancer provision Hetzner LBs via
  `load-balancer.hetzner.cloud/*` annotations.
- **CSI**: the `hcloud-csi` addon provides the `hcloud-volumes` StorageClass.
  It requires CCM-initialized nodes, so keep CCM enabled.

## 6. Networking

Two modes, controlled by `kommodity.network.ipv4.public`:

- **`public: true` (default)** - every node gets a public IPv4/IPv6. No
  private network is created. The Talos control-plane reconcilers reach nodes
  directly on port 50000.
- **`public: false`** - nodes get no public addresses and are attached to a
  Hetzner private network (`nodeCIDR` required, one network zone per network:
  eu-central, us-east, us-west, ap-southeast). Two caveats:
  - Hetzner private networks have **no managed NAT gateway**; without
    user-provided egress (e.g. a NAT instance, see
    [this Hetzner tutorial](https://community.hetzner.com/tutorials/terraform-hcloud-kubernetes-private))
    nodes cannot pull images.
  - The `kommodity.io/node-cidr` annotation (set automatically from
    `nodeCIDR`) routes Talos API traffic through the `talos-cluster-proxy`
    tunnel - keep that addon enabled.

## 7. API rate limits

Hetzner allows **3600 requests per hour per project**, replenishing one
request per second. Kommodity's CAPH reconcilers are wired with a token-bucket
work-queue limiter plus CAPH's own back-off (5-minute wait on rate-limit
responses), but the budget is shared by everything in the project:

- Use one project (and token) per set of clusters; don't co-locate unrelated
  API-heavy tooling in the same project.
- Tune cluster-autoscaler scan intervals if you run it - default settings are
  known to exhaust the limit.
- Higher limits can be requested from Hetzner support.

## 8. Costs

Billing is hourly with a monthly cap. Primary IPv4 addresses, load balancers,
snapshots, and volumes are billed separately. The integration test
(`make run-hetzner-integration-test`, requires `HCLOUD_TOKEN`) creates real
servers and a load balancer and **will incur costs**.

## 9. Teardown

Delete the `Cluster` resource (or `helm uninstall` + wait); CAPH finalizers
remove servers, the load balancer, the private network, and placement groups.
Verify with `hcloud server list`, `hcloud load-balancer list`,
`hcloud network list` that nothing leaked. CSI-created volumes whose PVCs
still exist at deletion time are not removed automatically - check
`hcloud volume list`.
