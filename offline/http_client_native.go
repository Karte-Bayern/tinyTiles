//go:build !js || !wasm

package offline

import "net/http"

var builtInHTTPFetcherClient = newBuiltInHTTPFetcherClient()

func newBuiltInHTTPFetcherClient() *http.Client {
	baseTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// An embedding application may replace the package-global transport
		// before this package initializes. Preserve that transport rather than
		// assuming implementation details and risking a startup panic.
		return http.DefaultClient
	}
	transport := baseTransport.Clone()
	// Synchronizer permits up to maxSyncConcurrency workers. Keeping that many
	// idle connections per origin avoids repeated TCP/TLS setup on later sync
	// ranges without imposing a maximum on active requests.
	transport.MaxIdleConns = maxSyncConcurrency * 2
	transport.MaxIdleConnsPerHost = maxSyncConcurrency
	return &http.Client{Transport: transport}
}

func defaultHTTPFetcherClient() *http.Client { return builtInHTTPFetcherClient }
