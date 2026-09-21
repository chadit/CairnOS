// Package nettest answers HTTP test fixtures inside the process instead of
// over a loopback socket. The host tracks loopback connections, so a fresh
// listener can bind an ephemeral port that connection tracking still holds
// stale state for, and the next SYN is dropped as invalid. Under parallel load
// that surfaces as "connect: no route to host" on a port this process is
// listening on.
package nettest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"
)

// BaseURL is the origin a single-handler fixture aims requests at. Nothing
// resolves .invalid, so a client that slips past the fixture transport fails
// at DNS rather than reaching a real host.
const BaseURL = "http://fixture.invalid"

// clientTimeout bounds one fixture request. Nothing dials, so this only
// catches a handler that blocks forever.
const clientTimeout = 30 * time.Second

// remoteAddr is the peer address handlers see. Real servers always set one,
// and code that splits it on a colon should not have to special-case the
// fixture.
const remoteAddr = "192.0.2.1:1234"

// Transport answers every request by running handler in this process. The
// caller's marshaling, redirect handling, status checks, and error wrapping
// all still run; only the socket is gone.
func Transport(handler http.Handler) http.RoundTripper {
	return handlerTransport{handler: handler}
}

// Client is an http.Client whose every request reaches handler.
func Client(handler http.Handler) *http.Client {
	return &http.Client{Transport: Transport(handler), Timeout: clientTimeout}
}

// handlerTransport runs the handler against a recorder and returns what it
// wrote. The response is complete before RoundTrip returns; a fixture that has
// to read a body while the handler still writes it wants Server instead.
type handlerTransport struct {
	handler http.Handler
}

// RoundTrip implements http.RoundTripper against the handler alone. A canceled
// request fails the way a real transport fails one, so the caller's deadline
// handling still gets exercised.
func (t handlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return serve(t.handler, request)
}

// serve runs one request through a handler and shapes the result the way a
// real transport does.
func serve(handler http.Handler, request *http.Request) (*http.Response, error) {
	if err := request.Context().Err(); err != nil {
		return nil, fmt.Errorf("%s %s: %w", request.Method, request.URL, err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, serverSide(request))

	if err := request.Context().Err(); err != nil {
		return nil, fmt.Errorf("%s %s: %w", request.Method, request.URL, err)
	}

	response := recorder.Result()
	response.Request = request

	return response, nil
}

// serverSide is the request as a handler expects to receive it: a body that is
// never nil, a peer address, and the RequestURI the server would have parsed.
// The caller's own request is left alone.
func serverSide(request *http.Request) *http.Request {
	served := request.Clone(request.Context())
	if served.Body == nil {
		served.Body = http.NoBody
	}

	served.RemoteAddr = remoteAddr
	served.RequestURI = served.URL.RequestURI()

	return served
}

// Unreachable answers every request with cause, standing in for a transport
// that never reached the host. It is how a fixture drives the dial-failure
// path without a port that has to stay closed.
func Unreachable(cause error) http.RoundTripper {
	return failingTransport{cause: cause}
}

// failingTransport is Unreachable's round tripper.
type failingTransport struct {
	cause error
}

// RoundTrip implements http.RoundTripper by failing.
func (t failingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("%s %s: %w", request.Method, request.URL, t.cause)
}
