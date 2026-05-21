// Package probe implements the AutoDiscoverProbe target-mode resolver.
// Given a container spec, it extracts the readiness HTTP path, port, and
// scheme so the controller can construct a load-generator URL that hits
// the same endpoint Kubernetes itself uses for readiness gating.
//
// The package is data-only — callers pass a corev1.Container struct,
// never a client. K8s discovery (fetching the Pod template from a
// Deployment / StatefulSet / DaemonSet) is the controller's job.
package probe

import (
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Spec is the resolved readiness endpoint, ready to feed into a
// loadgen URL builder.
type Spec struct {
	// Path is the HTTP path the readiness probe targets. Defaults to "/"
	// when the upstream HTTPGetAction omits Path.
	Path string
	// Port is the resolved numeric container port. Named ports are
	// looked up in container.Ports.
	Port int32
	// Scheme is "HTTP" or "HTTPS"; defaults to "HTTP".
	Scheme string
	// Host is the optional host override from HTTPGetAction.Host
	// (rarely set in real specs — Kubernetes defaults to the Pod IP).
	Host string
}

// DiscoverFromContainer extracts a Spec from container.ReadinessProbe.
// Returns an error if the container has no readiness probe, the probe is
// not an HTTPGet probe, or a named port cannot be resolved against
// container.Ports.
func DiscoverFromContainer(c corev1.Container) (Spec, error) {
	if c.ReadinessProbe == nil {
		return Spec{}, errors.New("container has no readinessProbe")
	}
	httpGet := c.ReadinessProbe.HTTPGet
	if httpGet == nil {
		return Spec{}, errors.New("readinessProbe is not HTTPGet (tcpSocket / exec / grpc probes are not supported by AutoDiscoverProbe)")
	}

	port, err := resolvePort(httpGet.Port, c.Ports)
	if err != nil {
		return Spec{}, err
	}

	path := httpGet.Path
	if path == "" {
		path = "/"
	}

	scheme := string(httpGet.Scheme)
	if scheme == "" {
		scheme = string(corev1.URISchemeHTTP)
	}

	return Spec{
		Path:   path,
		Port:   port,
		Scheme: scheme,
		Host:   httpGet.Host,
	}, nil
}

func resolvePort(p intstr.IntOrString, named []corev1.ContainerPort) (int32, error) {
	switch p.Type {
	case intstr.Int:
		if p.IntVal <= 0 {
			return 0, fmt.Errorf("readinessProbe.httpGet.port must be > 0, got %d", p.IntVal)
		}
		return p.IntVal, nil
	case intstr.String:
		if p.StrVal == "" {
			return 0, errors.New("readinessProbe.httpGet.port is an empty string")
		}
		for _, cp := range named {
			if cp.Name == p.StrVal {
				return cp.ContainerPort, nil
			}
		}
		return 0, fmt.Errorf("named port %q not found in container.ports", p.StrVal)
	default:
		return 0, fmt.Errorf("unknown port type %d", p.Type)
	}
}
