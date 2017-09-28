package metrics

import (
	"net/http"
	"time"

	"code.cloudfoundry.org/gorouter/route"
)

// Deprecated: this interface is marked for removal. It should be removed upon
// removal of Varz
//go:generate counterfeiter -o fakes/fake_varzreporter.go . VarzReporter
type VarzReporter interface {
	CaptureBadRequest()
	CaptureBadGateway()
	CaptureRoutingRequest(b *route.Endpoint)
	CaptureRoutingResponseLatency(b *route.Endpoint, statusCode int, t time.Time, d time.Duration)
}

//go:generate counterfeiter -o fakes/fake_proxyreporter.go . ProxyReporter
type ProxyReporter interface {
	CaptureBackendExhaustedConns()
	CaptureBackendInvalidID()
	CaptureBackendTLSHandshakeFailed()
	CaptureBadRequest()
	CaptureBadGateway()
	CaptureRoutingRequest(b *route.Endpoint)
	CaptureRoutingResponse(statusCode int)
	CaptureRoutingResponseLatency(b *route.Endpoint, d time.Duration)
	CaptureRouteServiceResponse(res *http.Response)
	CaptureWebSocketUpdate()
	CaptureWebSocketFailure()
}

type ComponentTagged interface {
	Component() string
}

//go:generate counterfeiter -o fakes/fake_registry_reporter.go . RouteRegistryReporter
type RouteRegistryReporter interface {
	CaptureRouteStats(totalRoutes int, msSinceLastUpdate uint64)
	CaptureRoutesPruned(prunedRoutes uint64)
	CaptureLookupTime(t time.Duration)
	CaptureRegistryMessage(msg ComponentTagged)
	CaptureUnregistryMessage(msg ComponentTagged)
}

//go:generate counterfeiter -o fakes/fake_combinedreporter.go . CombinedReporter
type CombinedReporter interface {
	CaptureBackendExhaustedConns()
	CaptureBackendInvalidID()
	CaptureBackendTLSHandshakeFailed()
	CaptureBadRequest()
	CaptureBadGateway()
	CaptureRoutingRequest(b *route.Endpoint)
	CaptureRoutingResponse(statusCode int)
	CaptureRoutingResponseLatency(b *route.Endpoint, statusCode int, t time.Time, d time.Duration)
	CaptureRouteServiceResponse(res *http.Response)
	CaptureWebSocketUpdate()
	CaptureWebSocketFailure()
}

type CompositeReporter struct {
	VarzReporter
	ProxyReporter
}

func (c *CompositeReporter) CaptureBadRequest() {
	c.VarzReporter.CaptureBadRequest()
	c.ProxyReporter.CaptureBadRequest()
}

func (c *CompositeReporter) CaptureBadGateway() {
	c.VarzReporter.CaptureBadGateway()
	c.ProxyReporter.CaptureBadGateway()
}

func (c *CompositeReporter) CaptureRoutingRequest(b *route.Endpoint) {
	c.VarzReporter.CaptureRoutingRequest(b)
	c.ProxyReporter.CaptureRoutingRequest(b)
}

func (c *CompositeReporter) CaptureRoutingResponseLatency(b *route.Endpoint, statusCode int, t time.Time, d time.Duration) {
	c.VarzReporter.CaptureRoutingResponseLatency(b, statusCode, t, d)
	c.ProxyReporter.CaptureRoutingResponseLatency(b, d)
}
