// Package analyzer hosts the static and runtime checkers that run alongside
// a ScaleValidation. Each sub-package owns one concern (cgroup throttling,
// HPA reaction latency, probe lag, DNS, PDB) and exposes:
//
//   - a pure data parser / state tracker with no Kubernetes API calls
//   - a typed report struct
//   - a Diagnostics() method that maps findings into
//     api/v1alpha1.DiagnosticAlert values for the controller to copy onto
//     ScaleValidation.status.diagnostics.
//
// Keeping the K8s client out of these packages keeps them unit-testable
// without envtest; the controller is responsible for fetching the inputs
// (cgroup file contents, HPA snapshots) and passing them in.
package analyzer
