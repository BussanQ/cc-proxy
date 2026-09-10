package helps

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestNewProxyAwareHTTPClientDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	client := NewProxyAwareHTTPClient(
		context.Background(),
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
		0,
	)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

// Recreating a client for each executor request must not recreate its pool.
func TestProxyAwareClientsReuseConnections(t *testing.T) {
	for _, factory := range []struct {
		name      string
		newClient func(context.Context, *config.Config, *cliproxyauth.Auth, time.Duration) *http.Client
	}{
		{"standard", NewProxyAwareHTTPClient},
		{"utls fallback", NewUtlsHTTPClient},
	} {
		for _, mode := range []string{"direct", "proxy"} {
			t.Run(factory.name+"/"+mode, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.WriteString(w, r.RemoteAddr)
				}))
				defer server.Close()
				proxyURL := "direct"
				target := server.URL
				if mode == "proxy" {
					proxyURL = server.URL
					target = "http://upstream.example/test"
				}
				auth := &cliproxyauth.Auth{ProxyURL: proxyURL}
				var firstAddr string
				for i := 0; i < 3; i++ {
					client := factory.newClient(t.Context(), nil, auth, 5*time.Second)
					defer client.CloseIdleConnections()
					resp, err := client.Get(target)
					if err != nil {
						t.Fatal(err)
					}
					body, err := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if err != nil {
						t.Fatal(err)
					}
					if i == 0 {
						firstAddr = string(body)
					} else if string(body) != firstAddr {
						t.Fatalf("request %d used connection %q, want %q", i, body, firstAddr)
					}
				}
			})
		}
	}
}

func TestProxyTransportCacheSettings(t *testing.T) {
	first := buildProxyTransport(" http://user:secret@127.0.0.1:29871 ")
	if first != buildProxyTransport("http://user:secret@127.0.0.1:29871") {
		t.Fatal("same normalized proxy did not share a transport")
	}
	if first == buildProxyTransport("http://user:other@127.0.0.1:29871") {
		t.Fatal("different proxy credentials shared a transport")
	}
	if buildProxyTransport("NONE") != buildProxyTransport(" direct ") {
		t.Fatal("direct aliases did not share a transport")
	}
	if first.MaxIdleConnsPerHost < upstreamMaxIdleConnsPerHost {
		t.Fatalf("idle pool too small: %d", first.MaxIdleConnsPerHost)
	}
}

func TestProxyAwareClientPreservesContextFallback(t *testing.T) {
	transport := utlsClientRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
	for _, proxyURL := range []string{"", "unsupported://proxy"} {
		client := NewProxyAwareHTTPClient(ctx, nil, &cliproxyauth.Auth{ProxyURL: proxyURL}, time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream.example", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if client.Timeout != time.Second {
			t.Fatalf("timeout = %v", client.Timeout)
		}
	}
}

func TestProxyAwareClientsReuseConcurrentConnections(t *testing.T) {
	const concurrency = 8
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	arrived := make(chan struct{}, concurrency)
	release := make(chan struct{}, concurrency)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		select {
		case <-release:
			_, _ = io.WriteString(w, r.RemoteAddr)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	auth := &cliproxyauth.Auth{ProxyURL: server.URL}
	defer NewProxyAwareHTTPClient(ctx, nil, auth, 0).CloseIdleConnections()
	connections := make(map[string]bool)
	type result struct {
		addr string
		err  error
	}
	for wave := 0; wave < 2; wave++ {
		results := make(chan result, concurrency)
		for i := 0; i < concurrency; i++ {
			go func() {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream.example/test", nil)
				if err != nil {
					results <- result{err: err}
					return
				}
				resp, err := NewProxyAwareHTTPClient(ctx, nil, auth, 0).Do(req)
				if err != nil {
					results <- result{err: err}
					return
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				results <- result{addr: string(body), err: err}
			}()
		}
		// Hold all responses until every connection is occupied so the second wave
		// must reuse the entire idle pool, rather than repeatedly borrowing one socket.
		for i := 0; i < concurrency; i++ {
			select {
			case <-arrived:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		for i := 0; i < concurrency; i++ {
			release <- struct{}{}
		}
		for i := 0; i < concurrency; i++ {
			got := <-results
			if got.err != nil {
				t.Fatal(got.err)
			}
			connections[got.addr] = true
		}
	}
	if len(connections) != concurrency {
		t.Fatalf("two waves opened %d connections, want %d", len(connections), concurrency)
	}
}
