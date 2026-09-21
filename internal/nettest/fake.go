package nettest

import (
	"fmt"
	"net/http"
)

// Fake stands in for an httptest.Server: aim requests at URL and send them
// with Client. No listener, port, or connection is involved, and the client
// answers from this fixture's handler whatever host a request carries, so a
// redirect the handler issues still lands back on it.
type Fake struct {
	handler http.Handler

	// URL is the origin to aim requests at. It resolves to nothing, so a client
	// built without Client fails at DNS instead of leaving the process.
	URL string
}

// New builds a fixture around handler. The host is derived from the fixture's
// own address, which keeps two fixtures in one test distinguishable without a
// package-level counter.
func New(handler http.Handler) *Fake {
	fake := &Fake{handler: handler}
	fake.URL = fmt.Sprintf("http://fixture-%p.invalid", fake)

	return fake
}

// Client reaches this fixture and nothing else.
func (f *Fake) Client() *http.Client {
	return Client(f.handler)
}

// Transport is Client's round tripper, for code that builds its own client.
func (f *Fake) Transport() http.RoundTripper {
	return Transport(f.handler)
}
