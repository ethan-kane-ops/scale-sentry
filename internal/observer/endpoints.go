package observer

import (
	"context"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/ethan-kane-ops/scale-sentry/internal/analyzer/leakage"
)

// endpointTracker diffs successive EndpointSlice states into per-IP
// Ready/Removed events. A Service can be backed by several slices, so the
// tracker keeps each slice's contribution and recomputes the union of
// ready IPs on every event.
type endpointTracker struct {
	slices map[string]map[string]bool // sliceName -> podIP -> ready
	ready  map[string]bool            // union of currently-ready IPs
}

func newEndpointTracker() *endpointTracker {
	return &endpointTracker{
		slices: map[string]map[string]bool{},
		ready:  map[string]bool{},
	}
}

// apply ingests one slice event and returns the resulting endpoint events.
// deleted=true means the whole slice was removed.
func (t *endpointTracker) apply(sliceName string, eps []discoveryv1.Endpoint, deleted bool, at time.Time) []leakage.EndpointEvent {
	if deleted {
		delete(t.slices, sliceName)
	} else {
		m := map[string]bool{}
		for _, ep := range eps {
			ready := ep.Conditions.Ready != nil && *ep.Conditions.Ready
			for _, ip := range ep.Addresses {
				m[ip] = m[ip] || ready
			}
		}
		t.slices[sliceName] = m
	}

	next := map[string]bool{}
	for _, m := range t.slices {
		for ip, ready := range m {
			if ready {
				next[ip] = true
			}
		}
	}

	var events []leakage.EndpointEvent
	for ip := range next {
		if !t.ready[ip] {
			events = append(events, leakage.EndpointEvent{At: at, PodIP: ip, Kind: leakage.EndpointReady})
		}
	}
	for ip := range t.ready {
		if !next[ip] {
			events = append(events, leakage.EndpointEvent{At: at, PodIP: ip, Kind: leakage.EndpointRemoved})
		}
	}
	t.ready = next
	return events
}

// watchEndpoints watches the target Service's EndpointSlices until ctx is
// cancelled, accumulating Ready/Removed events on the Session. A watch that
// closes without ctx cancellation is re-established.
func (s *Session) watchEndpoints(ctx context.Context) {
	tracker := newEndpointTracker()
	selector := discoveryv1.LabelServiceName + "=" + s.cfg.ServiceName
	for {
		w, err := s.clientset.DiscoveryV1().EndpointSlices(s.cfg.Namespace).
			Watch(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			warn("watch endpointslices: %v", err)
			return
		}
		if !s.consumeWatch(ctx, w, tracker) {
			return
		}
	}
}

// consumeWatch drains a watch until it closes. Returns false when ctx is
// done (stop), true when the channel merely closed (re-watch).
func (s *Session) consumeWatch(ctx context.Context, w watch.Interface, tracker *endpointTracker) bool {
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case ev, ok := <-w.ResultChan():
			if !ok {
				return true
			}
			slice, ok := ev.Object.(*discoveryv1.EndpointSlice)
			if !ok {
				continue
			}
			s.addEvents(tracker.apply(slice.Name, slice.Endpoints, ev.Type == watch.Deleted, time.Now()))
		}
	}
}
