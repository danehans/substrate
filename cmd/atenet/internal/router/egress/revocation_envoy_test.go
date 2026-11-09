//go:build envoyintegration

// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"golang.org/x/net/http2"
)

// Run with -tags envoyintegration after building Q6_ENVOY_IMAGE from this tree.
// An explicitly selected integration lane fails if Docker or its image is absent.
func TestEnvoyRevokesEstablishedConnections(t *testing.T) {
	for _, mode := range []string{"passthrough", "passthrough-tls", "mitm-http2", "passthrough-gateway-loss"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			actor := runningActor()
			actor.Status.ActivationId = "activation-1"
			var current atomic.Pointer[ateapi.Actor]
			current.Store(actor)
			api := &revocationAPI{get: func(context.Context) (*ateapi.Actor, error) { return current.Load(), nil }}
			r := newRevocations(api)
			token, err := r.admit(revocationCert(t), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			control := httptest.NewServer(r)
			defer control.Close()
			go r.run(ctx)
			var originPort string
			var listener net.Listener
			var httpOrigin *httptest.Server
			if strings.HasPrefix(mode, "passthrough") {
				listener, err = net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				if mode == "passthrough-tls" {
					server := httptest.NewTLSServer(http.NotFoundHandler())
					certificate := server.TLS.Certificates[0]
					server.Close()
					listener = tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}})
				}
				defer listener.Close()
				originPort = fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
				go func() {
					for {
						c, err := listener.Accept()
						if err != nil {
							return
						}
						go func() { defer c.Close(); io.Copy(c, c) }()
					}
				}()
			} else {
				httpOrigin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					w.WriteHeader(200)
					w.Write([]byte("open"))
					w.(http.Flusher).Flush()
					<-req.Context().Done()
				}))
				defer httpOrigin.Close()
				_, originPort, _ = net.SplitHostPort(strings.TrimPrefix(httpOrigin.URL, "http://"))
			}
			dir := t.TempDir()
			config, err := os.ReadFile("testdata/revocation-envoy.yaml")
			if err != nil {
				t.Fatal(err)
			}
			_, controlPort, _ := net.SplitHostPort(strings.TrimPrefix(control.URL, "http://"))
			innerTLS := ""
			innerFilter := `      - name: envoy.filters.network.tcp_proxy
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy
          stat_prefix: inner
          cluster: echo`
			if mode == "mitm-http2" {
				// Use the standard local test certificate only inside this fixture.
				certServer := httptest.NewTLSServer(http.NotFoundHandler())
				cert := certServer.TLS.Certificates[0]
				certServer.Close()
				key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
					t.Fatal(err)
				}
				innerTLS = `transport_socket:
        name: envoy.transport_sockets.tls
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext
          common_tls_context:
            alpn_protocols: [h2]
            tls_certificates:
            - certificate_chain: {filename: /q6/cert.pem}
              private_key: {filename: /q6/key.pem}`
				innerFilter = `      - name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: mitm
          codec_type: AUTO
          delayed_close_timeout: 0s
          route_config:
            name: origin
            virtual_hosts:
            - name: origin
              domains: ["*"]
              routes:
              - match: {prefix: /}
                route: {cluster: echo, timeout: 0s}
          http_filters:
          - name: envoy.filters.http.router
            typed_config: {"@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router}`
			}
			body := strings.NewReplacer("TOKEN_PLACEHOLDER", token, "IDENTITY_PLACEHOLDER", `{"actor_uid":"`+testEgressActorUID+`","activation":"activation-1","authority":"fixture"}`, "ORIGIN_PORT_PLACEHOLDER", originPort, "CONTROL_PORT_PLACEHOLDER", controlPort, "INNER_TLS_PLACEHOLDER", innerTLS, "INNER_FILTER_PLACEHOLDER", innerFilter).Replace(string(config))
			if err = os.WriteFile(filepath.Join(dir, "envoy.yaml"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			image := os.Getenv("Q6_ENVOY_IMAGE")
			if image == "" {
				image = "q6-atenet-envoy:latest"
			}
			out, err := exec.CommandContext(ctx, "docker", "run", "-d", "-p", "127.0.0.1::10000", "-v", dir+":/q6:ro", image, "-c", "/q6/envoy.yaml", "--concurrency", "1").CombinedOutput()
			if err != nil {
				t.Fatalf("start Envoy: %v %s", err, out)
			}
			container := strings.TrimSpace(string(out))
			defer func() {
				exec.Command("docker", "stop", container).Run()
				exec.Command("docker", "container", "rm", container).Run()
			}()
			var address string
			for {
				port, err := exec.CommandContext(ctx, "docker", "port", container, "10000/tcp").CombinedOutput()
				if err == nil && len(port) > 0 {
					address = strings.TrimSpace(strings.Split(string(port), "\n")[0])
					break
				}
				logs, _ := exec.CommandContext(ctx, "docker", "logs", container).CombinedOutput()
				if strings.Contains(string(logs), "error initializing") {
					t.Fatalf("Envoy config: %s", logs)
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			for {
				logs, _ := exec.CommandContext(ctx, "docker", "logs", container).CombinedOutput()
				if strings.Contains(string(logs), "starting workers") {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatalf("Envoy startup: %s", logs)
				case <-time.After(10 * time.Millisecond):
				}
			}
			connection, err := net.DialTimeout("tcp", address, 2*time.Second)
			if err != nil {
				logs, _ := exec.Command("docker", "logs", container).CombinedOutput()
				t.Fatalf("dial %s: %v %s", address, err, logs)
			}
			defer connection.Close()
			connection.SetDeadline(time.Now().Add(15 * time.Second))
			fmt.Fprint(connection, "CONNECT fixture:443 HTTP/1.1\r\nHost: fixture:443\r\n\r\n")
			reader := bufio.NewReader(connection)
			request := &http.Request{Method: "CONNECT"}
			response, err := http.ReadResponse(reader, request)
			if err != nil || response.StatusCode != 200 {
				logs, _ := exec.Command("docker", "logs", container).CombinedOutput()
				t.Fatalf("CONNECT: %v %#v %s", err, response, logs)
			}
			if mode == "passthrough-tls" {
				secure := tls.Client(connection, &tls.Config{InsecureSkipVerify: true}) // Fixture certificate only.
				if err = secure.HandshakeContext(ctx); err != nil {
					t.Fatal(err)
				}
				connection = secure
				reader = bufio.NewReader(connection)
			}
			var bodies []io.ReadCloser
			if strings.HasPrefix(mode, "passthrough") {
				connection.Write([]byte("open"))
				got := make([]byte, 4)
				if _, err = io.ReadFull(reader, got); err != nil || string(got) != "open" {
					t.Fatalf("existing bytes: %q %v", got, err)
				}
			} else {
				tlsConnection := tls.Client(connection, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}}) // Fixture certificate only.
				if err = tlsConnection.HandshakeContext(ctx); err != nil {
					t.Fatal(err)
				}
				transport := &http2.Transport{}
				client, err := transport.NewClientConn(tlsConnection)
				if err != nil {
					t.Fatal(err)
				}
				for range 2 {
					req, _ := http.NewRequestWithContext(ctx, "GET", "https://fixture/open", nil)
					response, err := client.RoundTrip(req)
					if err != nil {
						t.Fatal(err)
					}
					got := make([]byte, 4)
					if _, err = io.ReadFull(response.Body, got); err != nil || string(got) != "open" {
						t.Fatalf("MITM in-flight: %q %v", got, err)
					}
					bodies = append(bodies, response.Body)
				}
			}
			quarantined := runningActor()
			quarantined.Status.ActivationId = "activation-1"
			quarantined.Status.Quarantine = &ateapi.ActorQuarantine{RequestId: "q1"}
			placed := time.Now()
			if mode == "passthrough-gateway-loss" {
				if out, err := exec.CommandContext(ctx, "docker", "kill", container).CombinedOutput(); err != nil {
					t.Fatalf("kill selected gateway: %v %s", err, out)
				}
			} else {
				current.Store(quarantined)
			}
			// The actor remains RUNNING and this fixture never asks the worker to stop.
			if strings.HasPrefix(mode, "passthrough") {
				_, err = reader.ReadByte()
				if err == nil {
					t.Fatal("passthrough remained open")
				}
			} else {
				for _, body := range bodies {
					_, err = body.Read(make([]byte, 1))
					body.Close()
					if err == nil {
						t.Fatal("HTTP/2 stream remained open")
					}
				}
			}
			elapsed := time.Since(placed)
			if elapsed > 8*time.Second {
				t.Fatalf("close budget exceeded: %s", elapsed)
			}
			logs, _ := exec.Command("docker", "logs", container).CombinedOutput()
			if mode != "passthrough-gateway-loss" && (!strings.Contains(string(logs), "connect_revocation reason=actor-quarantined") || !strings.Contains(string(logs), "activation-1")) {
				t.Fatalf("missing attributed socket close: %s", logs)
			}
			if strings.Contains(string(logs), token) {
				t.Fatal("control capability leaked to logs")
			}
			t.Logf("A=1 T=1 R=1 mode=%s close=%s GetActor=%d", mode, elapsed, api.calls.Load())
		})
	}
}
