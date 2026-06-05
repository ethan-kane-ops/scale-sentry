# Load Profiles

Scale Sentry's loadgen runs an ordered list of **phases**. Each phase carries its own arrival pattern, rate, and duration. The default (single-phase, constant-rate) is what older `ScaleValidation` CRs land on, but `spec.load.profile` unlocks open-loop and multi-phase shapes that exercise the autoscaler more realistically.

## Why open-loop matters

Closed-loop benchmarks (one request, wait for response, send the next) **hide** the moments when the target stalls: a 200ms backend pause just means each worker sends fewer requests, and the latency histogram still looks healthy. Open-loop generators emit requests on a schedule regardless of how fast the target replies, so a stall shows up as growing queue depth and elevated tail latency. That is the failure mode an HPA needs to react to.

The scale-sentry loadgen also applies the **scheduled-arrival fix** for coordinated omission: every request's latency is measured from the slot's scheduled time, not from the moment the worker actually dispatched it. A stalled target turns into honest p99 growth instead of silently-absorbed queue wait.

## Patterns

`spec.load.profile.pattern` selects the arrival shape. Pattern-specific knobs apply only to their named pattern; the rest are ignored.

| Pattern    | Shape                                                                | Use when                                                                  |
|------------|----------------------------------------------------------------------|---------------------------------------------------------------------------|
| `Constant` | Fixed rate (legacy default)                                          | Smoke testing; comparing against pre-loadprofile baselines                |
| `Poisson`  | Exponential inter-arrival, mean `1 / baseRps`                        | SLA-accurate verdicts; matches real user traffic                          |
| `Ramp`     | Linear interpolation `baseRps` → `endRps` over `rampDuration`        | Measuring HPA reaction latency under monotonic growth                     |
| `Step`     | Climbs `stepRps` every `stepDuration`, holding each plateau          | Stressing HPA stabilization windows and downscaling hysteresis            |
| `Spike`    | Decomposed into Constant base / spike / base / ... by the controller | Reproducing diurnal traffic spikes and noisy-neighbour patterns           |

```yaml
spec:
  load:
    baseRps: 200
    concurrencyFactor: 50
    profile:
      pattern: Ramp
      endRps: 800
      rampDuration: 3m
```

## Warmup phase

`spec.load.warmupDuration` runs traffic against the target before the SLA window opens. Requests are dispatched (so TCP/TLS handshakes settle, JIT runs, page caches warm) but their latencies and counters are **excluded** from the verdict. The warmup phase always runs at `baseRps` with the Constant pattern.

```yaml
spec:
  load:
    baseRps: 200
    concurrencyFactor: 50
    warmupDuration: 30s   # 30s warmup then the measurement window
    profile:
      pattern: Poisson
```

Skip the warmup (omit `warmupDuration`) when comparing against the older single-shot defaults; otherwise enable it for any verdict that needs to survive code review.

## Spike profile

The `Spike` pattern stitches one or more elevated-rate windows into a `baseRps` Constant background. The controller decomposes the spike into atomic phases before sending them to the loadgen, so the SLA verdict reflects each slice individually.

```yaml
spec:
  load:
    baseRps: 100
    concurrencyFactor: 50
    profile:
      pattern: Spike
      spikes:
        - at: 30s        # offset from measurement start
          duration: 10s
          rps: 600
        - at: 90s
          duration: 5s
          rps: 1200
```

`at` is measured from the start of the measurement phase (post-warmup). Spike RPS must exceed `baseRps`; otherwise it would be a dip, which is rejected by the CRD.

## Phase summaries in the result

Every executed phase contributes a `PhaseSummary` to the run output:

```json
{
  "phases": [
    { "name": "warmup",  "pattern": "Constant", "recordStats": false, "started": "...", "ended": "..." },
    { "name": "measure", "pattern": "Poisson",  "recordStats": true,  "started": "...", "ended": "..." }
  ],
  "warmupDuration":      "30s",
  "measurementDuration": "3m0s"
}
```

`WarmupDuration` and `MeasurementDuration` sum the wall-clock spent in each kind of phase. Use them to confirm a long run actually entered the measurement window before drawing verdicts.
