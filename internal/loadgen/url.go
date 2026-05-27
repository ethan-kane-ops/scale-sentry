package loadgen

import (
	"errors"
	"fmt"
	"strings"
)

// URLSpec describes the inputs needed to construct a target URL for the
// ServiceDefault/CustomPath × ClusterIP/Ingress matrix.
//
// The builder is pure, it does not call the Kubernetes API. The controller
// is responsible for filling in [URLSpec.ServiceName], [URLSpec.Namespace],
// [URLSpec.IngressHost], [URLSpec.CustomPath] and [URLSpec.Port] from the
// resolved Deployment/Service/Ingress objects.
type URLSpec struct {
	TargetMode  TargetMode
	NetworkPath NetworkPath

	// ServiceName and Namespace are required when NetworkPath == ClusterIP.
	// The resulting host is <ServiceName>.<Namespace>.svc.cluster.local.
	ServiceName string
	Namespace   string

	// IngressHost is required when NetworkPath == Ingress. Pre-resolved
	// from the matching Ingress rule, e.g. "checkout.example.com".
	IngressHost string

	// CustomPath is the HTTP path component. Required when TargetMode is
	// CustomPath or AutoDiscoverProbe. Must start with "/". When empty for
	// ServiceDefault, defaults to "/".
	CustomPath string

	// Port is the destination TCP port. Required when NetworkPath == ClusterIP.
	// For Ingress, defaults to 80/443 based on scheme.
	Port int

	// Scheme overrides the default scheme. Default: http for ClusterIP,
	// https for Ingress.
	Scheme string
}

// Build returns the absolute URL or an error if required fields are missing.
func (s URLSpec) Build() (string, error) {
	switch s.NetworkPath {
	case PathClusterIP:
		return s.buildClusterIP()
	case PathIngress:
		return s.buildIngress()
	case "":
		return "", errors.New("networkPath is required")
	default:
		return "", fmt.Errorf("unknown networkPath %q", s.NetworkPath)
	}
}

func (s URLSpec) buildClusterIP() (string, error) {
	if s.ServiceName == "" {
		return "", errors.New("serviceName is required for ClusterIP")
	}
	if s.Namespace == "" {
		return "", errors.New("namespace is required for ClusterIP")
	}
	if s.Port <= 0 {
		return "", errors.New("port is required for ClusterIP")
	}
	path, err := resolvePath(s.TargetMode, s.CustomPath)
	if err != nil {
		return "", err
	}
	scheme := s.Scheme
	if scheme == "" {
		scheme = "http"
	}
	host := fmt.Sprintf("%s.%s.svc.cluster.local", s.ServiceName, s.Namespace)
	return fmt.Sprintf("%s://%s:%d%s", scheme, host, s.Port, path), nil
}

func (s URLSpec) buildIngress() (string, error) {
	if s.IngressHost == "" {
		return "", errors.New("ingressHost is required for Ingress")
	}
	path, err := resolvePath(s.TargetMode, s.CustomPath)
	if err != nil {
		return "", err
	}
	scheme := s.Scheme
	if scheme == "" {
		scheme = "https"
	}
	host := s.IngressHost
	if s.Port > 0 {
		host = fmt.Sprintf("%s:%d", host, s.Port)
	}
	return fmt.Sprintf("%s://%s%s", scheme, host, path), nil
}

func resolvePath(mode TargetMode, custom string) (string, error) {
	switch mode {
	case TargetServiceDefault:
		if custom == "" {
			return "/", nil
		}
		if !strings.HasPrefix(custom, "/") {
			return "", fmt.Errorf("customPath must start with /, got %q", custom)
		}
		return custom, nil
	case TargetCustomPath, TargetAutoDiscover:
		if custom == "" {
			return "", fmt.Errorf("customPath is required for targetMode %q", mode)
		}
		if !strings.HasPrefix(custom, "/") {
			return "", fmt.Errorf("customPath must start with /, got %q", custom)
		}
		return custom, nil
	case "":
		return "", errors.New("targetMode is required")
	default:
		return "", fmt.Errorf("unknown targetMode %q", mode)
	}
}
