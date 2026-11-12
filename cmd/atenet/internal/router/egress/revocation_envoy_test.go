//go:build envoyintegration

// Copyright 2026 The Agent Substrate Authors
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
	"errors"
	"fmt"
	"google.golang.org/protobuf/proto"
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
	cases := []struct{ mode, activation, trigger string }{}
	for _, activation := range []string{"", "activation-1"} {
		for _, mode := range []string{"passthrough", "passthrough-tls", "mitm-http2", "passthrough-gateway-loss", "idle-connect", "partial-clienthello"} {
			cases = append(cases, struct{ mode, activation, trigger string }{mode, activation, "quarantine"})
		}
	}
	for _, trigger := range []string{"uid", "deleted", "outage", "slow", "mixed-rotation", "mixed-quarantine"} {
		cases = append(cases, struct{ mode, activation, trigger string }{"passthrough", "", trigger})
	}
	for _, tc := range cases {
		mode, activation := tc.mode, tc.activation
		t.Run(mode+"/"+activation+"/"+tc.trigger, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			actor := runningActor()
			actor.Status.ActivationId = "activation-1"
			var current atomic.Pointer[ateapi.Actor]
			current.Store(actor)
			var unavailable, slow atomic.Bool
			api := &revocationAPI{get: func(ctx context.Context) (*ateapi.Actor, error) {
				if slow.Load() {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				if unavailable.Load() {
					return nil, errors.New("ateapi unavailable")
				}
				a := current.Load()
				if a == nil {
					return nil, errors.New("actor deleted")
				}
				return a, nil
			}}
			r := newRevocations(api)
			token, err := r.admit(revocationCertWithActivation(t, activation), time.Now())
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
			if mode == "mitm-http2" || mode == "idle-connect" || mode == "partial-clienthello" {
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
			identity := `{"actor_uid":"` + testEgressActorUID + `","activation_id":"` + activation + `","authority":"fixture"}`
			var second string
			if strings.HasPrefix(tc.trigger, "mixed-") {
				second, err = r.admit(revocationCert(t), time.Now())
				if err != nil {
					t.Fatal(err)
				}
				parts := strings.SplitN(string(config), "\n  - name: inner", 2)
				outer := strings.SplitN(parts[0], "  - name: outer", 2)[1]
				extra := "  - name: outer" + outer
				extra = strings.NewReplacer("name: outer", "name: outer-bound", "port_value: 10000", "port_value: 10001", "TOKEN_PLACEHOLDER", second, "IDENTITY_PLACEHOLDER", `{"actor_uid":"`+testEgressActorUID+`","activation_id":"activation-1","authority":"fixture"}`).Replace(extra)
				config = []byte(parts[0] + "\n" + extra + "\n  - name: inner" + parts[1])
			}
			body := strings.NewReplacer("TOKEN_PLACEHOLDER", token, "IDENTITY_PLACEHOLDER", identity, "ORIGIN_PORT_PLACEHOLDER", originPort, "CONTROL_PORT_PLACEHOLDER", controlPort, "INNER_TLS_PLACEHOLDER", innerTLS, "INNER_FILTER_PLACEHOLDER", innerFilter).Replace(string(config))
			if err = os.WriteFile(filepath.Join(dir, "envoy.yaml"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			image := os.Getenv("Q6_ENVOY_IMAGE")
			if image == "" {
				image = "q6-atenet-envoy:latest"
			}
			out, err := exec.CommandContext(ctx, "docker", "run", "-d", "-p", "127.0.0.1::10000", "-p", "127.0.0.1::10001", "-v", dir+":/q6:ro", image, "-c", "/q6/envoy.yaml", "--concurrency", "1").CombinedOutput()
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
			connection.SetDeadline(time.Now().Add(30 * time.Second))
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
			var mixedBound net.Conn
			var mixedReader *bufio.Reader
			var bodies []io.ReadCloser
			if strings.HasPrefix(mode, "passthrough") {
				connection.Write([]byte("open"))
				got := make([]byte, 4)
				if _, err = io.ReadFull(reader, got); err != nil || string(got) != "open" {
					t.Fatalf("existing bytes: %q %v", got, err)
				}
			} else if mode == "mitm-http2" {
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
			if mode == "partial-clienthello" {
				connection.Write([]byte{0x16, 0x03})
			}
			if strings.HasPrefix(tc.trigger, "mixed-") {
				ports, err := exec.CommandContext(ctx, "docker", "port", container, "10001/tcp").CombinedOutput()
				if err != nil {
					t.Fatal(err)
				}
				bound, err := net.DialTimeout("tcp", strings.TrimSpace(string(ports)), 2*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer bound.Close()
				bound.SetDeadline(time.Now().Add(25 * time.Second))
				fmt.Fprint(bound, "CONNECT fixture:443 HTTP/1.1\r\nHost: fixture:443\r\n\r\n")
				boundReader := bufio.NewReader(bound)
				mixedBound, mixedReader = bound, boundReader
				response, err := http.ReadResponse(boundReader, request)
				if err != nil || response.StatusCode != 200 {
					t.Fatalf("bound CONNECT: %v", err)
				}
				echo := func(c net.Conn, reader *bufio.Reader) {
					t.Helper()
					if _, err := c.Write([]byte("live")); err != nil {
						t.Fatal(err)
					}
					got := make([]byte, 4)
					if _, err := io.ReadFull(reader, got); err != nil || string(got) != "live" {
						t.Fatalf("tunnel not live: %v", err)
					}
				}
				echo(bound, boundReader)
				held := proto.Clone(actor).(*ateapi.Actor)
				held.Status.Holds = []*ateapi.ActorHold{{Holder: "ordinary"}}
				current.Store(held)
				waitForEnvoyActorObservation(t, ctx, r, token, time.Now())
				echo(connection, reader)
				echo(bound, boundReader)
				r.mu.Lock()
				groups := len(r.actors)
				shared := r.tunnels[token].check == r.tunnels[second].check
				r.mu.Unlock()
				if groups != 1 || !shared {
					t.Fatal("mixed tunnels created a second poll group")
				}
				if tc.trigger == "mixed-rotation" {
					rotated := proto.Clone(held).(*ateapi.Actor)
					rotated.Status.ActivationId = "activation-2"
					current.Store(rotated)
					bound.SetDeadline(time.Now().Add(8 * time.Second))
					if _, err := boundReader.ReadByte(); err == nil || isTimeout(err) {
						t.Fatalf("bound tunnel did not close on rotation: %v", err)
					}
					echo(connection, reader)
				}
			}
			quarantined := runningActor()
			quarantined.Status.ActivationId = "activation-1"
			quarantined.Status.Quarantine = &ateapi.ActorQuarantine{RequestId: "q1"}
			placed := time.Now()
			connection.SetDeadline(placed.Add(8 * time.Second))
			if mixedBound != nil {
				mixedBound.SetDeadline(placed.Add(8 * time.Second))
			}
			if mode == "passthrough-gateway-loss" {
				if out, err := exec.CommandContext(ctx, "docker", "kill", container).CombinedOutput(); err != nil {
					t.Fatalf("kill selected gateway: %v %s", err, out)
				}
			} else {
				switch tc.trigger {
				case "uid":
					quarantined.Metadata.Uid = "replacement"
					current.Store(quarantined)
				case "deleted":
					current.Store(nil)
				case "outage":
					unavailable.Store(true)
				case "slow":
					slow.Store(true)
					// Start this RPC promptly so its timeout is observed before the
					// independent freshness watchdog reaches the same deadline.
					r.mu.Lock()
					r.tunnels[token].check.due = time.Now()
					r.mu.Unlock()
				default:
					current.Store(quarantined)
				}
			}
			// The actor remains RUNNING and this fixture never asks the worker to stop.
			if mode == "idle-connect" || mode == "partial-clienthello" {
				_, err = io.Copy(io.Discard, reader)
				if err != nil {
					t.Fatalf("pending handshake close: %v", err)
				}
			} else if strings.HasPrefix(mode, "passthrough") {
				_, err = reader.ReadByte()
				if err == nil || isTimeout(err) {
					t.Fatalf("passthrough remained open: %v", err)
				}
			} else {
				for _, body := range bodies {
					_, err = body.Read(make([]byte, 1))
					body.Close()
					if err == nil || isTimeout(err) {
						t.Fatalf("HTTP/2 stream remained open: %v", err)
					}
				}
			}
			if tc.trigger == "mixed-quarantine" {
				if _, err := mixedReader.ReadByte(); err == nil || isTimeout(err) {
					t.Fatalf("quarantine left bound tunnel open: %v", err)
				}
				mixedBound.Close()
			}
			elapsed := time.Since(placed)
			if elapsed > 8*time.Second {
				t.Fatalf("close budget exceeded: %s", elapsed)
			}
			logs, _ := exec.Command("docker", "logs", container).CombinedOutput()
			reason := "actor-quarantined"
			switch tc.trigger {
			case "uid":
				reason = "actor-uid-mismatch"
			case "deleted", "outage", "slow":
				reason = "actor-check-failed"
			}
			attributed := false
			for _, line := range strings.Split(string(logs), "\n") {
				if strings.Contains(line, "connect_revocation reason="+reason) && strings.Contains(line, `"activation_id":"`+activation+`"`) {
					attributed = true
				}
			}
			if mode != "passthrough-gateway-loss" && !attributed {
				t.Fatalf("missing attributed socket close: %s", logs)
			}
			if strings.Contains(string(logs), token) || second != "" && strings.Contains(string(logs), second) {
				t.Fatal("control capability leaked to logs")
			}
			if strings.HasPrefix(tc.trigger, "mixed-") && permit(r, second).Code != 403 {
				t.Fatal("quarantine did not close both mixed handles")
			}
			t.Logf("A=1 R=1 mode=%s activation=%q trigger=%s close=%s GetActor=%d", mode, activation, tc.trigger, elapsed, api.calls.Load())
		})
	}
}

func isTimeout(err error) bool {
	var e net.Error
	return errors.As(err, &e) && e.Timeout()
}

func waitForEnvoyActorObservation(t *testing.T, ctx context.Context, r *revocations, token string, after time.Time) {
	t.Helper()
	for {
		r.mu.Lock()
		checked := r.tunnels[token].check.checked
		r.mu.Unlock()
		if !checked.Before(after) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
