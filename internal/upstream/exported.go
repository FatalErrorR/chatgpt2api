package upstream

import (
	"context"
	"io"

	http "github.com/bogdanfinn/fhttp"
)

// The compat layer (internal/chatgptweb) builds the Python version's request bodies itself and
// sends them through this client's transport: browser fingerprint, headers, account proxy and the
// sentinel tokens. These wrappers expose exactly that.

// Bootstrap loads chatgpt.com once per 6 hours to learn the build numbers.
func (u *OpenAIBackendAPI) Bootstrap(ctx context.Context) error { return u.bootstrap(ctx) }

// ChatRequirements runs the sentinel prepare/finalize handshake; later requests carry its tokens.
func (u *OpenAIBackendAPI) ChatRequirements(ctx context.Context) error {
	return u.chatRequirements(ctx)
}

// Request sends one backend request with the browser headers. Non-2xx answers become
// *UpstreamHTTPError (401 becomes ErrTokenInvalidated).
func (u *OpenAIBackendAPI) Request(ctx context.Context, method, path string, payload any, extra map[string]string) (*http.Response, error) {
	return u.request(ctx, method, path, payload, extra)
}

// RequestBody is Request with a raw body.
func (u *OpenAIBackendAPI) RequestBody(ctx context.Context, method, path string, body io.Reader, contentType string, extra map[string]string) (*http.Response, error) {
	return u.requestBody(ctx, method, path, body, contentType, extra)
}

// Do sends a request to any URL (signed upload and download links) through the account's proxy,
// without the backend headers. The caller checks the status.
func (u *OpenAIBackendAPI) Do(req *http.Request) (*http.Response, error) {
	client, err := u.proxy.TLSClient(u.accountProxy, false)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// BaseURL is chatgpt.com (or the fake upstream in tests).
func (u *OpenAIBackendAPI) BaseURL() string { return u.baseURL }

// UserAgent is the browser user agent the client presents.
func (u *OpenAIBackendAPI) UserAgent() string { return upstreamUserAgent }

// AccountProxy is the proxy this client uses ("" for the global/runtime one).
func (u *OpenAIBackendAPI) AccountProxy() string { return u.accountProxy }

// AccessToken is the account's token ("" for the anonymous routes).
func (u *OpenAIBackendAPI) AccessToken() string { return u.accessToken }
