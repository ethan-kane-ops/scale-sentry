# High Availability

The controller uses `controller-runtime` leader election (a `coordination.k8s.io/Lease` in the release namespace), so it is safe to run more than one replica. Only the leader reconciles; standbys idle until the lease expires.

Enable HA by raising the replica count:

```bash
helm upgrade --install scale-sentry oci://ghcr.io/ethan-kane-ops/charts/scale-sentry \
  --set controller.replicaCount=2
```

With `replicaCount > 1` the chart also renders a `PodDisruptionBudget` (`minAvailable: 1`) and zone `topologySpreadConstraints`.

## Tradeoff

HA adds one Lease object and a brief reconcile gap on failover. When the leader pod dies, a standby acquires the lease within the renew window (~15s default) before resuming.

A single replica is fine for non-prod. Set `--leader-elect=false` (or `controller.leaderElect=false`) for local single-node dev to skip the Lease entirely.
