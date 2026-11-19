package server

import "net/http"

// idleConnsPerHost covers the most requests a test keeps in flight to one
// server. With the default of two, a test's parallel callers close a
// connection on almost every call, and on Windows the loopback ports left in
// TIME_WAIT can run out mid-run, failing dials with "address in use".
const idleConnsPerHost = 64

// testClient is what the tests call the server with.
var testClient = &http.Client{Transport: testTransport()}

func testTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = idleConnsPerHost
	return tr
}
