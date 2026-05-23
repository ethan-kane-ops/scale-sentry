# Scale Sentry: Bottlenecks, Logic Bugs, and Architectural Improvements

This document lists critical bugs, architectural gaps, and performance bottlenecks identified in the current state of **`scale-sentry`**. These issues should be resolved to achieve a production-grade, highly efficient, and fault-tolerant autoscaling and traffic resilience validator.

---


---

## 2. [LOGIC BUG] Pre-Stress "Newest Pod" Timing Flaw in Probe Lag Resolution
* **Component:** `internal/observer/observer.go` & `internal/observer/target.go`
* **Severity:** High
* **Problem:** The `probelag` analyzer measures the readiness transition conditions of a pre-existing, healthy pod instead of the newly created pods spawned during the autoscaling event.
* **Root Cause:**
  1. At the very beginning of the run (before load generation starts or HPA scaling happens), `Session.Run` calls `resolveTarget`.
  2. `resolveTarget` lists the existing pods and stores the latest one in `target.newestPod`.
  3. At finalization, `collectProbeLag` pulls the status for `t.newestPod` and computes probe lag.
  4. Consequently, the analyzer measures the conditions of a pod that was *already running and ready* before the test even started, completely missing the newly spawned replicas!
* **Remediation:**
  Refactor `collectProbeLag` in `internal/observer/target.go` to:
  1. Retrieve a fresh pod list at finalization time.
  2. Filter for pods created *after* the run start timestamp (`start := time.Now()`).
  3. Select the newest pod from this filtered list to run the `probelag` analysis on a true scale-up replica.

---

## 3. [PERFORMANCE] Rate Limiter Mutex Lock Contention under High RPS
* **Component:** `internal/loadgen/generator.go`
* **Severity:** High
* **Problem:** At high target RPS, concurrent workers spend excessive CPU cycles competing for the rate limiter's internal mutex, causing lock contention that distorts client-side latency metrics.
* **Root Cause:**
  A single `rate.Limiter` instance is shared across all concurrent workers. In the worker loop, each worker calls `g.limiter.Wait(ctx)`, acquiring and releasing a single mutex:
  ```go
  func (g *Generator) worker(ctx context.Context, wg *sync.WaitGroup, c *collector) {
      defer wg.Done()
      for {
          if err := g.limiter.Wait(ctx); err != nil {
              return // ctx done
          }
          g.do(ctx, c)
      }
  }
  ```
* **Remediation:**
  Implement **striped/worker-local rate limiting**.
  1. Divide the target RPS and burst budget by the concurrency factor: `workerRPS = targetRPS / concurrency` and `workerBurst = targetBurst / concurrency`.
  2. Instantiate a separate, dedicated `rate.Limiter` for each worker goroutine.
  3. Workers call their local limiter without sharing states or competing for locks.

---

## 4. [PERFORMANCE/MEM] Unbounded Latency Memory Allocation & Sorting Complexity
* **Component:** `internal/loadgen/generator.go` & `internal/loadgen/result.go`
* **Severity:** High
* **Problem:** Appending every request duration to an unbounded slice consumes massive amounts of memory under load and triggers a heavy $O(M \log M)$ sorting step at finalization.
* **Root Cause:**
  The `collector` accumulates every request duration in a raw slice:
  ```go
  c.latencies = append(c.latencies, latency)
  ```
  For high-throughput, long-duration runs (e.g., 50,000 RPS for 5 minutes = 15 million requests), this slice consumes hundreds of megabytes of RAM and takes seconds to sort during finalization via `sort.Slice` in the `percentiles` function. This can easily trigger out-of-memory (OOM) crashes in the observer container.
* **Remediation:**
  Replace the unbounded `[]time.Duration` slice with an **HDR Histogram** (e.g., using a Go implementation like `github.com/HdrHistogram/hdrhistogram-go`) or a pre-allocated bucketing structure.
  * Captures latency percentiles (p50, p95, p99) in $O(1)$ constant time and memory space.
  * Eliminates the finalization sorting phase completely.

---

## 5. [PERFORMANCE] $O(E \times N)$ Nested Loop in Traffic Leakage and Drain Correlation
* **Component:** `internal/analyzer/leakage/leakage.go` & `internal/analyzer/drain/drain.go`
* **Severity:** Medium
* **Problem:** Correlating error samples against EndpointSlice events using nested loops scales quadratically, which will lag the observer execution thread on high-failure runs.
* **Root Cause:**
  The `Correlate` functions in both analyzers loop over all sorted errors and, for each error, loop over all ready/removed events:
  ```go
  for _, errSample := range sortedErrors {
      assigned := false
      for i, ev := range readyEvents {
          if errSample.At.Before(ev.At) { continue }
          if errSample.At.Sub(ev.At) >= leakageWindow { continue }
          correlated[i].Errors = append(correlated[i].Errors, errSample)
          assigned = true
          break
      }
      ...
  }
  ```
* **Remediation:**
  Since both `readyEvents` and `sortedErrors` are pre-sorted chronologically, replace the nested loop with a **single-pass sliding window (two-pointer approach)**.
  * Reduces time complexity from $O(E \times N)$ to an extremely efficient $O(E + N)$ linear pass.

---

## 6. [FAULT TOLERANCE] Distroless Failures and High-Risk RBAC in cgroup exec Scraping
* **Component:** `internal/observer/exec.go`
* **Severity:** Medium
* **Problem:** Executing `cat /sys/fs/cgroup/cpu.stat` inside the target container fails on minimal base images and requires security-sensitive RBAC permissions.
* **Root Cause:**
  1. **Shell Dependency:** Minimally packaged containers (built `FROM scratch` or `distroless`) do not contain a shell or the `cat` binary. Executing `cat` in these containers returns `exec: "cat": executable file not found in $PATH`.
  2. **RBAC Security Risks:** Spawning pod execs requires the `pods/exec` RBAC verb, which represents a high security risk and is blocked by default in secure, hardened Kubernetes environments.
* **Remediation:**
  Transition away from `pods/exec` toward Kubelet-level or node-level scraping:
  * Query the **Kubelet Stats Summary API** (`/stats/summary`) from the observer/controller. The `/stats/summary` endpoint provides container CPU stats (including `cpu.cfs.throttled_periods_pct` or raw periods/throttled values) securely without container dependencies or privileged exec permissions.

---

## 7. [SYSTEM CAPABILITY] TLS Hardcoding & Lack of Custom CA Support
* **Component:** `internal/loadgen/client.go`
* **Severity:** Medium
* **Problem:** The load generator cannot test endpoints using self-signed certificates or custom CAs (very common in dev/Kind environments or Service Mesh setups).
* **Root Cause:**
  The `TLSConfig` in `newClient` is hardcoded to a bare-bones configuration without customizable parameters:
  ```go
  TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
  ```
  This causes requests to fail with `x509: certificate signed by unknown authority` when encountering self-signed certs.
* **Remediation:**
  1. Add `InsecureSkipVerify bool` and `CABundle []byte` to the `TargetConfig` / `LoadConfig` API.
  2. Update `newClient` to configure `InsecureSkipVerify: cfg.InsecureSkipVerify` and append custom certificates to the client's `RootCAs` pool.

---

## 8. [ACCURACY] Init Container Runtime Skews Scheduling Latency
* **Component:** `internal/analyzer/probelag/probelag.go`
* **Severity:** Low
* **Problem:** The calculated `SchedulingLatency` is artificially inflated by the execution duration of init containers.
* **Root Cause:**
  `SchedulingLatency` is computed as `diff(r.Scheduled, r.Initialized)` where `PodInitialized` transitions to True only after all init containers finish. If a pod has heavy schema migrations, sidecar bootstrapping, or asset warming init containers, their run duration is miscategorized as queue scheduling lag.
* **Remediation:**
  Isolate these metrics clearly into two separate measurements:
  * `TrueSchedulingLatency` = `Scheduled - CreationTimestamp` (actual time waiting in scheduler queues).
  * `InitContainerRuntime` = `Initialized - Scheduled` (actual execution time of init container chains).

---

## 9. [TIMING RELIABILITY] Informer Watch Propagation Lag
* **Component:** `internal/observer/endpoints.go`
* **Severity:** Low
* **Problem:** Endpoint Ready and Removed event timestamps are assigned using `time.Now()` at event reception, introducing latency skews due to informer watch propagation delays.
* **Root Cause:**
  ```go
  s.addEvents(tracker.apply(slice.Name, slice.Endpoints, ev.Type == watch.Deleted, time.Now()))
  ```
  If the API server or network path is congested, the observer might receive the watch event seconds after the transition actually occurred in the cluster, artificially extending or shifting the leakage/drain windows.
* **Remediation:**
  While individual endpoints in `EndpointSlice` do not carry transit timestamps, the observer can track its own informer queue delay by comparing event metadata times or monitoring API server event rate to warn the user when metrics are likely skewed due to watch lag.
