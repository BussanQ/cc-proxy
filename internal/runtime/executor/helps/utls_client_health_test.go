package helps

import (
	"context"
	standardtls "crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/net/http2"
)

func TestUtlsRoundTripperHTTP2HealthCheck(t *testing.T) {
	for _, acknowledge := range []bool{false, true} {
		name := "unresponsive"
		if acknowledge {
			name = "responsive"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				transport := newUtlsRoundTripper("").http2Transport()
				if transport.ReadIdleTimeout <= 0 || transport.PingTimeout <= 0 || transport.ReadIdleTimeout+transport.PingTimeout >= transport.IdleConnTimeout {
					t.Fatal("health check must finish before idle connection expiry")
				}
				client, server := net.Pipe()
				defer client.Close()
				defer server.Close()
				pings := make(chan struct{}, 1)
				go func() {
					framer := http2.NewFramer(server, server)
					preface := make([]byte, len(http2.ClientPreface))
					if _, err := io.ReadFull(server, preface); err != nil {
						return
					}
					for {
						frame, err := framer.ReadFrame()
						if err != nil {
							return
						}
						if ping, ok := frame.(*http2.PingFrame); ok && !ping.IsAck() {
							pings <- struct{}{}
							if acknowledge {
								if err := framer.WritePing(true, ping.Data); err != nil {
									return
								}
							}
						}
					}
				}()
				// Write server SETTINGS independently so neither side blocks the
				// other's initial writes on the unbuffered pipe.
				go func() { _ = http2.NewFramer(server, nil).WriteSettings() }()
				conn, err := transport.NewClientConn(client)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				synctest.Wait()
				// Sleep advances virtual time inside this synctest bubble.
				time.Sleep(transport.ReadIdleTimeout)
				synctest.Wait()
				select {
				case <-pings:
				default:
					t.Fatal("no HTTP/2 health-check PING sent")
				}
				time.Sleep(transport.PingTimeout)
				synctest.Wait()
				if closed := conn.State().Closed; closed == acknowledge {
					t.Fatalf("connection closed = %v, ping acknowledged = %v", closed, acknowledge)
				}
			})
		})
	}
}

func TestUtlsRoundTripperTLSClientConfig(t *testing.T) {
	certificate := newResumptionTestCertificate(t)
	roots := x509.NewCertPool()
	roots.AddCert(certificate.Leaf)
	verifyErr := errors.New("certificate rejected by callback")
	for _, tt := range []struct {
		name    string
		config  *standardtls.Config
		wantErr error
	}{
		{
			name:   "custom roots and server name",
			config: &standardtls.Config{ServerName: "api.anthropic.com", RootCAs: roots},
		},
		{
			name:   "skip verification",
			config: &standardtls.Config{ServerName: "untrusted.example", InsecureSkipVerify: true},
		},
		{
			name: "extra TLS options are ignored",
			config: &standardtls.Config{
				ServerName: "api.anthropic.com", RootCAs: roots,
				MinVersion:                  standardtls.VersionTLS12,
				NextProtos:                  []string{"custom"},
				DynamicRecordSizingDisabled: true,
			},
		},
		{
			name: "verification callback",
			config: &standardtls.Config{
				ServerName: "api.anthropic.com", RootCAs: roots,
				VerifyPeerCertificate: func(_ [][]byte, _ [][]*x509.Certificate) error { return verifyErr },
			},
			wantErr: verifyErr,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			originalNextProtos := slices.Clone(tt.config.NextProtos)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.TLS.ServerName != tt.config.ServerName || r.ProtoMajor != 2 {
					t.Errorf("SNI = %q, protocol = %q", r.TLS.ServerName, r.Proto)
				}
				_, _ = io.WriteString(w, "ok")
			}))
			server.EnableHTTP2 = true
			server.TLS = &standardtls.Config{Certificates: []standardtls.Certificate{certificate}}
			server.StartTLS()
			defer server.Close()
			roundTripper := &utlsRoundTripper{dialer: contextDialerFunc(func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			})}
			roundTripper.http2Transport().TLSClientConfig = tt.config
			defer roundTripper.CloseIdleConnections()
			client := &http.Client{Transport: roundTripper, Timeout: 5 * time.Second}
			resp, err := client.Get("https://chatgpt.com/")
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("request error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil || string(body) != "ok" {
				t.Fatalf("response = %q, error = %v", body, err)
			}
			if !slices.Equal(tt.config.NextProtos, originalNextProtos) {
				t.Fatal("caller TLS config was mutated")
			}
		})
	}
}
