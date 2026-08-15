# Hetzner Cloud

Kommodity runs Talos workload clusters on Hetzner Cloud through the
[Syself cluster-api-provider-hetzner](https://github.com/syself/cluster-api-provider-hetzner)
(CAPH). Cloud servers only — Hetzner Robot bare metal is not wired up.

## What you need

- A Hetzner Cloud **project**. The API token and the rate limit are both
  project-scoped, so use one project per set of related clusters.
- A **read/write API token** for it (Console → Security → API tokens).
- A **Talos snapshot** in the project (below).

## Talos image

Hetzner has no custom-image upload, so Talos ships as a snapshot. Build the
image with the [`talos-cloud-image` workflow](../.github/workflows/talos-cloud-image.yml)
(platform `hcloud`), then upload it with
[hcloud-upload-image](https://github.com/apricote/hcloud-upload-image):

```bash
hcloud-upload-image upload \
  --image-path kommodity-talos-hcloud-v1.13.7.raw \
  --architecture x86 \
  --description kommodity-talos-hcloud-v1.13.7 \
  --labels caph-image-name=kommodity-talos-hcloud-v1.13.7
```

The `caph-image-name` label is mandatory — snapshots have no name, so CAPH
resolves `imageName` through that label. ARM (CAX) server types need a
separate `--architecture arm` snapshot with its own image name.

Plain factory images work too:

```bash
--image-url https://factory.talos.dev/image/<schematic>/v1.13.7/hcloud-amd64.raw.xz --compression xz
```

## Deploying a cluster

Create the token Secret (CAPH reads key `hcloud`):

```bash
kubectl --kubeconfig kommodity.yaml create secret generic hetzner \
  --from-literal=hcloud=$HCLOUD_TOKEN
```

Then deploy:

```bash
helm template my-cluster charts/kommodity-cluster \
  -f charts/kommodity-cluster/values.hetzner.yaml \
  | kubectl --kubeconfig kommodity.yaml apply -f -
```

[`values.hetzner.yaml`](../charts/kommodity-cluster/values.hetzner.yaml) has
the full option set: `region` (fsn1, nbg1, hel1, ash, hil, sin), `sku`,
replicas, load balancer type, image name. Root disk size is fixed by the
server type — there is no disk knob.

The chart creates a `HetznerCluster` with an hcloud load balancer (`lb11` by
default) fronting the control plane, one `HCloudMachineTemplate` per pool, and
delivers the hcloud CCM and CSI driver as addons.

## Networking

Set by `kommodity.network.ipv4.public`:

- **`true` (default)** — every node gets a public IPv4/IPv6, no private
  network.
- **`false`** — nodes are private-only on a Hetzner network (`nodeCIDR`
  required). Two caveats: Hetzner private networks have **no managed NAT**, so
  nodes cannot pull images without your own egress; and the
  `kommodity.io/node-cidr` annotation routes Talos API traffic through the
  `talos-cluster-proxy` addon, which must stay enabled.

## Rate limits

Hetzner allows **3600 requests/hour per project**, refilling one per second.
The CAPH reconcilers run behind a token-bucket work-queue limiter plus CAPH's
own 5-minute back-off, but the budget is shared with everything else in the
project — including cluster-autoscaler, whose default scan interval is known
to exhaust it. Higher limits can be requested from Hetzner support.

## Costs and teardown

Servers, load balancers, primary IPv4s, snapshots, and volumes all bill
separately. `make run-hetzner-integration-test` creates real infrastructure
and costs real money.

To tear down, delete the `Cluster` (or `helm uninstall` and wait) — CAPH
finalizers remove servers, the load balancer, the network, and placement
groups. CSI volumes whose PVCs still exist are *not* removed; check
`hcloud volume list` afterwards.
