//go:build js && wasm

package offline

import "net/http"

// Browser requests are implemented by Fetch; transport connection-pool knobs
// do not apply there.
func defaultHTTPFetcherClient() *http.Client { return http.DefaultClient }
