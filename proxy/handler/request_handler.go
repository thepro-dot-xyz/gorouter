package handler

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	router_http "code.cloudfoundry.org/gorouter/common/http"
	"code.cloudfoundry.org/gorouter/handlers"
	"code.cloudfoundry.org/gorouter/logger"
	"code.cloudfoundry.org/gorouter/metrics"
	"code.cloudfoundry.org/gorouter/proxy/utils"
	"code.cloudfoundry.org/gorouter/route"
	"github.com/uber-go/zap"
)

const (
	MaxRetries = 3
)

var NoEndpointsAvailable = errors.New("No endpoints available")

type RequestHandler struct {
	logger   logger.Logger
	reporter metrics.ProxyReporter

	request  *http.Request
	response utils.ProxyResponseWriter

	endpointDialTimeout time.Duration

	tlsConfigTemplate *tls.Config

	forwarder              *Forwarder
	disableXFFLogging      bool
	disableSourceIPLogging bool
}

func NewRequestHandler(request *http.Request, response utils.ProxyResponseWriter, r metrics.ProxyReporter, logger logger.Logger, endpointDialTimeout time.Duration, tlsConfig *tls.Config, opts ...func(*RequestHandler)) *RequestHandler {
	reqHandler := &RequestHandler{
		reporter:            r,
		request:             request,
		response:            response,
		endpointDialTimeout: endpointDialTimeout,
		tlsConfigTemplate:   tlsConfig,
	}

	for _, option := range opts {
		option(reqHandler)
	}

	requestLogger := setupLogger(reqHandler.disableXFFLogging, reqHandler.disableSourceIPLogging, request, logger)
	reqHandler.forwarder = &Forwarder{
		BackendReadTimeout: endpointDialTimeout, // TODO: different values?
		Logger:             requestLogger,
	}
	reqHandler.logger = requestLogger

	return reqHandler
}

func setupLogger(disableXFFLogging, disableSourceIPLogging bool, request *http.Request, logger logger.Logger) logger.Logger {
	fields := []zap.Field{
		zap.String("RemoteAddr", request.RemoteAddr),
		zap.String("Host", request.Host),
		zap.String("Path", request.URL.Path),
		zap.Object("X-Forwarded-For", request.Header["X-Forwarded-For"]),
		zap.Object("X-Forwarded-Proto", request.Header["X-Forwarded-Proto"]),
	}
	// Specific indexes below is to preserve the schema in the log line
	if disableSourceIPLogging {
		fields[0] = zap.String("RemoteAddr", "-")
	}

	if disableXFFLogging {
		fields[3] = zap.Object("X-Forwarded-For", "-")
	}

	l := logger.Session("request-handler").With(fields...)
	return l
}

func DisableXFFLogging(t bool) func(*RequestHandler) {
	return func(h *RequestHandler) {
		h.disableXFFLogging = t
	}
}

func DisableSourceIPLogging(t bool) func(*RequestHandler) {
	return func(h *RequestHandler) {
		h.disableSourceIPLogging = t
	}
}

func (h *RequestHandler) Logger() logger.Logger {
	return h.logger
}

func (h *RequestHandler) HandleBadGateway(err error, request *http.Request) {
	h.reporter.CaptureBadGateway()

	handlers.AddRouterErrorHeader(h.response, "endpoint_failure")

	h.writeStatus(http.StatusBadGateway, "Registered endpoint failed to handle the request.")
	h.response.Done()
}

func (h *RequestHandler) HandleTcpRequest(iter route.EndpointIterator) {
	h.logger.Info("handling-tcp-request", zap.String("Upgrade", "tcp"))

	onConnectionFailed := func(err error) { h.logger.Error("tcp-connection-failed", zap.Error(err)) }
	backendStatusCode, err := h.serveTcp(iter, nil, onConnectionFailed)
	if err != nil {
		h.logger.Error("tcp-request-failed", zap.Error(err))
		h.writeStatus(http.StatusBadGateway, "TCP forwarding to endpoint failed.")
		return
	}
	h.response.SetStatus(backendStatusCode)
}

func (h *RequestHandler) HandleWebSocketRequest(iter route.EndpointIterator) {
	h.logger.Info("handling-websocket-request", zap.String("Upgrade", "websocket"))

	onConnectionSucceeded := func(connection net.Conn, endpoint *route.Endpoint) error {
		h.setupRequest(endpoint)
		err := h.request.Write(connection)
		if err != nil {
			return err
		}
		return nil
	}
	onConnectionFailed := func(err error) { h.logger.Error("websocket-connection-failed", zap.Error(err)) }

	backendStatusCode, err := h.serveTcp(iter, onConnectionSucceeded, onConnectionFailed)

	if err != nil {
		h.logger.Error("websocket-request-failed", zap.Error(err))
		h.writeStatus(http.StatusBadGateway, "WebSocket request to endpoint failed.")
		h.reporter.CaptureWebSocketFailure()
		return
	}
	h.response.SetStatus(backendStatusCode)
	h.reporter.CaptureWebSocketUpdate()
}

func (h *RequestHandler) writeStatus(code int, message string) {
	body := fmt.Sprintf("%d %s: %s", code, http.StatusText(code), message)

	h.logger.Info("status", zap.String("body", body))

	http.Error(h.response, body, code)
	if code > 299 {
		h.response.Header().Del("Connection")
	}
}

type connSuccessCB func(net.Conn, *route.Endpoint) error
type connFailureCB func(error)

var nilConnSuccessCB = func(net.Conn, *route.Endpoint) error { return nil }
var nilConnFailureCB = func(error) {}

func (h *RequestHandler) serveTcp(
	iter route.EndpointIterator,
	onConnectionSucceeded connSuccessCB,
	onConnectionFailed connFailureCB,
) (int, error) {
	var err error
	var backendConnection net.Conn
	var endpoint *route.Endpoint

	if onConnectionSucceeded == nil {
		onConnectionSucceeded = nilConnSuccessCB
	}
	if onConnectionFailed == nil {
		onConnectionFailed = nilConnFailureCB
	}

	dialer := &net.Dialer{
		Timeout: h.endpointDialTimeout, // untested
	}

	retry := 0
	for {
		endpoint = iter.Next()
		if endpoint == nil {
			err = NoEndpointsAvailable
			h.HandleBadGateway(err, h.request)
			return 0, err
		}

		iter.PreRequest(endpoint)

		if endpoint.IsTLS() {
			tlsConfigLocal := utils.TLSConfigWithServerName(endpoint.ServerCertDomainSAN, h.tlsConfigTemplate)
			backendConnection, err = tls.DialWithDialer(dialer, "tcp", endpoint.CanonicalAddr(), tlsConfigLocal)
		} else {
			backendConnection, err = net.DialTimeout("tcp", endpoint.CanonicalAddr(), h.endpointDialTimeout)
		}

		iter.PostRequest(endpoint)
		if err == nil {
			break
		}

		iter.EndpointFailed(err)
		onConnectionFailed(err)

		retry++
		if retry == MaxRetries {
			return 0, err
		}
	}
	if backendConnection == nil {
		return 0, nil
	}
	defer backendConnection.Close()

	err = onConnectionSucceeded(backendConnection, endpoint)
	if err != nil {
		return 0, err
	}

	client, _, err := h.hijack()
	if err != nil {
		return 0, err
	}
	defer client.Close()

	backendStatusCode := h.forwarder.ForwardIO(client, backendConnection)
	return backendStatusCode, nil
}

func (h *RequestHandler) setupRequest(endpoint *route.Endpoint) {
	h.setRequestURL(endpoint.CanonicalAddr())
	h.setRequestXForwardedFor()
	SetRequestXRequestStart(h.request)
}

func (h *RequestHandler) setRequestURL(addr string) {
	h.request.URL.Scheme = "http"
	h.request.URL.Host = addr
}

func (h *RequestHandler) setRequestXForwardedFor() {
	if clientIP, _, err := net.SplitHostPort(h.request.RemoteAddr); err == nil {
		// If we aren't the first proxy retain prior
		// X-Forwarded-For information as a comma+space
		// separated list and fold multiple headers into one.
		if prior, ok := h.request.Header["X-Forwarded-For"]; ok {
			clientIP = strings.Join(prior, ", ") + ", " + clientIP
		}
		h.request.Header.Set("X-Forwarded-For", clientIP)
	}
}

func SetRequestXRequestStart(request *http.Request) {
	if _, ok := request.Header[http.CanonicalHeaderKey("X-Request-Start")]; !ok {
		request.Header.Set("X-Request-Start", strconv.FormatInt(time.Now().UnixNano()/1e6, 10))
	}
}

func SetRequestXCfInstanceId(request *http.Request, endpoint *route.Endpoint) {
	value := endpoint.PrivateInstanceId
	if value == "" {
		value = endpoint.CanonicalAddr()
	}

	request.Header.Set(router_http.CfInstanceIdHeader, value)
}

func (h *RequestHandler) hijack() (client net.Conn, io *bufio.ReadWriter, err error) {
	return h.response.Hijack()
}
