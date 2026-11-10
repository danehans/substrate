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
	"bytes"
	"context"
	"encoding/pem"
	"fmt"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/internal/resources"
	"google.golang.org/grpc"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type revocationAPI struct {
	ateapi.ControlClient
	get   func(context.Context) (*ateapi.Actor, error)
	calls atomic.Int64
}

func (a *revocationAPI) GetActor(ctx context.Context, _ *ateapi.GetActorRequest, _ ...grpc.CallOption) (*ateapi.Actor, error) {
	a.calls.Add(1)
	return a.get(ctx)
}
func revocationCert(t *testing.T) certifiedActor {
	return revocationCertWithActivation(t, "activation-1")
}
func revocationCertWithActivation(t *testing.T, activation string) certifiedActor {
	t.Helper()
	ca := newTestCA(t, "revocation-ca")
	return certifiedActor{ref: resources.ActorRef{Atespace: testEgressAtespace, Name: testEgressActor}, leaf: ca.issueActorCert(t, "spiffe://substrate-actor.local/worker-for-actor/default/my-actor", actorCertOptions{activation: activation})}
}
func permit(r *revocations, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/permit", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
func TestRevocationChecks(t *testing.T) {
	for _, activation := range []string{"", "activation-1"} {
		for _, tc := range []struct {
			name, reason string
			mutate       func(*ateapi.Actor)
			fail         bool
			slow         bool
		}{
			{name: "ordinary hold", mutate: func(a *ateapi.Actor) { a.Status.Holds = []*ateapi.ActorHold{{Holder: "ordinary"}} }},
			{name: "quarantine while RUNNING", reason: "actor-quarantined", mutate: func(a *ateapi.Actor) { a.Status.Quarantine = &ateapi.ActorQuarantine{RequestId: "q1"} }},
			{name: "UID before quarantine", reason: "actor-uid-mismatch", mutate: func(a *ateapi.Actor) {
				a.Metadata.Uid = "replacement"
				a.Status.Quarantine = &ateapi.ActorQuarantine{RequestId: "q1"}
			}},
			{name: "activation rotation", reason: "activation-mismatch", mutate: func(a *ateapi.Actor) { a.Status.ActivationId = "later" }},
			{name: "deleted", reason: "actor-check-failed", fail: true},
			{name: "ateapi outage", reason: "actor-check-failed", fail: true},
			{name: "slow GetActor", reason: "actor-check-failed", slow: true},
		} {
			t.Run(activation+"/"+tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					actor := runningActor()
					actor.Status.ActivationId = "activation-1"
					if tc.mutate != nil {
						tc.mutate(actor)
					}
					api := &revocationAPI{get: func(ctx context.Context) (*ateapi.Actor, error) {
						if tc.fail {
							return nil, fmt.Errorf("unavailable")
						}
						if tc.slow {
							<-ctx.Done()
							return nil, ctx.Err()
						}
						return actor, nil
					}}
					r := newRevocations(api)
					token, err := r.admit(revocationCertWithActivation(t, activation), time.Now())
					if err != nil {
						t.Fatal(err)
					}
					if got := permit(r, token).Code; got != 200 {
						t.Fatalf("initial permit: %d", got)
					}
					group := r.tunnels[token].check
					r.check(context.Background(), r.tunnels[token].key.actor(), group)
					if activation == "" && tc.reason == "activation-mismatch" {
						tc.reason = ""
					}
					response := permit(r, token)
					if tc.reason == "" {
						if response.Code != 200 {
							t.Fatal(response.Body.String())
						}
					} else if response.Code != 403 || strings.TrimSpace(response.Body.String()) != tc.reason {
						t.Fatalf("got %d %q want %s", response.Code, response.Body.String(), tc.reason)
					}
				})
			})
		}
	}
}

func TestRevocationReleaseReconnectKeepsOldHandlesRevoked(t *testing.T) {
	r := newRevocations(nil)
	cert := revocationCert(t)
	old, err := r.admit(cert, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	permit(r, old)
	r.tunnels[old].check.reason = "actor-quarantined"
	fresh, err := r.admit(cert, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if permit(r, fresh).Code != 200 || permit(r, old).Code != 403 {
		t.Fatal("reconnect inherited old revocation or reopened old handle")
	}
}
func TestRevocationBatchesLiveTunnels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		actor := runningActor()
		actor.Status.ActivationId = "activation-1"
		api := &revocationAPI{get: func(context.Context) (*ateapi.Actor, error) { return actor, nil }}
		r := newRevocations(api)
		withActivation := revocationCert(t)
		withoutActivation := revocationCertWithActivation(t, "")
		tokens := make([]string, 10)
		for i := range tokens {
			var err error
			cert := withActivation
			if i%2 == 0 {
				cert = withoutActivation
			}
			tokens[i], err = r.admit(cert, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			permit(r, tokens[i])
		}
		r.tunnels[tokens[0]].check.due = time.Now().Add(4900 * time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go r.run(ctx)
		for range 12 {
			time.Sleep(500 * time.Millisecond)
			for _, token := range tokens {
				if permit(r, token).Code != 200 {
					t.Fatal("permit failed")
				}
			}
		}
		if api.calls.Load() != 1 {
			t.Fatalf("A=1 T=10 R=1: got %d GetActor calls in6s, want1", api.calls.Load())
		}
		for _, token := range tokens {
			req := httptest.NewRequest(http.MethodDelete, "/permit", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			r.ServeHTTP(httptest.NewRecorder(), req)
		}
		time.Sleep(6 * time.Second)
		if api.calls.Load() != 1 {
			t.Fatal("polled without live handles")
		}
	})
}
func TestRevocationNeverRefreshesAfterFreshnessDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		actor := runningActor()
		actor.Status.ActivationId = "activation-1"
		api := &revocationAPI{get: func(context.Context) (*ateapi.Actor, error) { time.Sleep(time.Second); return actor, nil }}
		r := newRevocations(api)
		token, err := r.admit(revocationCert(t), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		permit(r, token)
		time.Sleep(6500 * time.Millisecond)
		r.check(context.Background(), r.tunnels[token].key.actor(), r.tunnels[token].check)
		if permit(r, token).Code != 403 {
			t.Fatal("late success refreshed expired authorization")
		}
	})
}
func TestRevocationControlRejectsUnissuedCapability(t *testing.T) {
	r := newRevocations(nil)
	if permit(r, strings.Repeat("a", 64)).Code != 403 {
		t.Fatal("accepted unissued capability")
	}
	if len(r.actors) != 0 {
		t.Fatal("control request created identity")
	}
}

func TestRevocationPendingHandleSurvivesUntilFirstPermit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		actor := runningActor()
		actor.Status.ActivationId = "activation-1"
		actor.Status.Quarantine = &ateapi.ActorQuarantine{RequestId: "q"}
		api := &revocationAPI{get: func(context.Context) (*ateapi.Actor, error) { return actor, nil }}
		r := newRevocations(api)
		token, err := r.admit(revocationCert(t), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go r.run(ctx)
		time.Sleep(time.Second)
		if api.calls.Load() != 0 {
			t.Fatal("polled before a handle became live")
		}
		if permit(r, token).Code != 200 {
			t.Fatal("pending handle lost")
		}
		for range 10 {
			time.Sleep(500 * time.Millisecond)
			permit(r, token)
		}
		if got := permit(r, token); got.Code != 403 || !strings.Contains(got.Body.String(), "actor-quarantined") {
			t.Fatalf("lost pending poll group: %d %s", got.Code, got.Body.String())
		}
	})
}
func TestConnectRevocationMetadataComesFromVerifiedCertificate(t *testing.T) {
	for _, activation := range []string{"", "activation-1"} {
		t.Run(activation, func(t *testing.T) {
			ca := newTestCA(t, "actor-ca")
			leaf := ca.issueActorCert(t, "spiffe://substrate-actor.local/worker-for-actor/default/my-actor", actorCertOptions{activation: activation})
			actor := runningActor()
			actor.Status.ActivationId = activation
			h := egressHandler(ca.loadRoots, actor, nil)
			result, err := h.HandleRequestHeaders(context.Background(), egressMetadata(xfccHeader(leaf)))
			if err != nil {
				t.Fatal(err)
			}
			fields := result.DynamicMetadata.GetFields()["dev.ate.egress"].GetStructValue().GetFields()

			token := fields["revocation_token"].GetStringValue()
			if fields["revocation_mode"].GetStringValue() != "required" || len(token) != 64 {
				t.Fatal("missing exact-run permit")
			}
			binding := fields["revocation_identity"].GetStringValue()
			if !strings.Contains(binding, testEgressActorUID) || !strings.Contains(binding, `"activation_id":"`+activation+`"`) {
				t.Fatal("close attribution lost verified binding")
			}
			if permit(h.revocations, token).Code != 200 {
				t.Fatal("CONNECT did not register capability")
			}
		})
	}
}

func TestRevocationScaleMeasurements(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const actors, tunnels, replicas = 1000, 10, 3
		actor := runningActor()
		actor.Status.ActivationId = "activation-1"
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var registries []*revocations
		for replica := range replicas {
			api := &revocationAPI{get: func(context.Context) (*ateapi.Actor, error) { return actor, nil }}
			r := newRevocations(api)
			registries = append(registries, r)
			for i := range actors {
				key := executionKey{"default", fmt.Sprint(i), testEgressActorUID, "activation-1"}
				checked := time.Now()
				group := &actorCheck{checked: checked, due: checked.Add(time.Duration((i*17+replica*97)%4900) * time.Millisecond), tunnels: make(map[*tunnelPermit]struct{})}
				r.actors[key.actor()] = group
				for tunnel := range tunnels {
					token := fmt.Sprintf("%064x", i*tunnels+tunnel)
					permit := &tunnelPermit{key: key, accessed: checked, active: true, check: group}
					r.tunnels[token] = permit
					group.tunnels[permit] = struct{}{}
				}
			}
			go r.run(ctx)
		}
		for range 120 {
			time.Sleep(500 * time.Millisecond)
			for _, r := range registries {
				r.mu.Lock()
				for _, tunnel := range r.tunnels {
					tunnel.accessed = time.Now()
				}
				r.mu.Unlock()
			}
		}
		var lookups, overdue int64
		for _, r := range registries {
			lookups += r.lookups.Load()
			overdue += r.overdue.Load()
		}
		t.Logf("synthetic instant-ateapi A=%d T=%d R=%d duration=60s GetActor=%d QPS=%.2f overdue=%d", actors, tunnels, replicas, lookups, float64(lookups)/60, overdue)
		if overdue != 0 {
			t.Fatalf("synthetic scheduling freshness failures: %d", overdue)
		}
	})
}
func TestRevocationPollBacklogFailsClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &revocationAPI{get: func(ctx context.Context) (*ateapi.Actor, error) { <-ctx.Done(); return nil, ctx.Err() }}
		r := newRevocations(api)
		now := time.Now()
		for i := range 100 {
			key := executionKey{"default", fmt.Sprint(i), "uid", "activation"}
			group := &actorCheck{checked: now, due: now}
			r.actors[key.actor()] = group
			r.tunnels[fmt.Sprintf("%064x", i)] = &tunnelPermit{key: key, accessed: now, active: true, check: group}
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go r.run(ctx)
		time.Sleep(7500 * time.Millisecond)
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, group := range r.actors {
			if group.reason == "" {
				t.Fatal("backlog retained expired authorization")
			}
		}
		if r.lookups.Load() > 100 {
			t.Fatal("lookups multiplied by backlog")
		}
	})
}

func TestAgentgatewayConnectDoesNotAllocateEnvoyPermit(t *testing.T) {
	ca := newTestCA(t, "actor-ca")
	leaf := ca.issueActorCert(t, "spiffe://substrate-actor.local/worker-for-actor/default/my-actor", actorCertOptions{activation: "activation-1"})
	actor := runningActor()
	actor.Status.ActivationId = "activation-1"
	h := egressHandler(ca.loadRoots, actor, nil)
	certificate := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw}))
	result, err := h.HandleRequestHeaders(context.Background(), agentgatewayEgressMetadata(certificate))
	if err != nil {
		t.Fatal(err)
	}
	fields := result.DynamicMetadata.GetFields()["dev.ate.egress"].GetStructValue().GetFields()
	if len(h.revocations.tunnels) != 0 || fields["revocation_token"] != nil || fields["revocation_mode"] != nil {
		t.Fatal("agentgateway allocated an unconsumed Envoy permit")
	}
}

func TestFreshAdmissionDoesNotReuseExpiredPendingGroup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRevocations(nil)
		cert := revocationCert(t)
		old, err := r.admit(cert, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(8 * time.Second)
		fresh, err := r.admit(cert, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if permit(r, fresh).Code != 200 || permit(r, old).Code != 403 {
			t.Fatal("fresh admission reused expired authorization")
		}
	})
}

func TestRevocationPhaseCleanupPreservesOtherHandle(t *testing.T) {
	r := newRevocations(nil)
	token, err := r.admit(revocationCert(t), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, phase string) int {
		req := httptest.NewRequest(method, "/permit", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Permit-Phase", phase)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	if request(http.MethodPost, "connect") != 200 || request(http.MethodPost, "network") != 200 {
		t.Fatal("phase registration failed")
	}
	if request(http.MethodDelete, "network") != 204 || request(http.MethodPost, "connect") != 200 {
		t.Fatal("inner cleanup invalidated outer CONNECT")
	}
	if len(r.tunnels) != 1 || len(r.actors) != 1 {
		t.Fatal("phase handles duplicated actor/token registry")
	}
	if request(http.MethodDelete, "connect") != 204 || request(http.MethodPost, "network") != 403 {
		t.Fatal("retired token reopened")
	}
}

func TestRevocationControlCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name             string
		authorization    func(string) string
		method, path     string
		retired, expired bool
	}{
		{name: "missing", authorization: func(string) string { return "" }},
		{name: "wrong scheme", authorization: func(token string) string { return "Basic " + token }},
		{name: "short", authorization: func(string) string { return "Bearer a" }},
		{name: "nonhex", authorization: func(string) string { return "Bearer " + strings.Repeat("z", 64) }},
		{name: "unknown", authorization: func(string) string { return "Bearer " + strings.Repeat("a", 64) }},
		{name: "reused after retirement", retired: true},
		{name: "expired without cleanup", expired: true},
		{name: "wrong path", path: "/other"},
		{name: "retarget by query", path: "/permit?actor=other"},
		{name: "wrong method", method: http.MethodGet},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRevocations(nil)
			token, err := r.admit(revocationCert(t), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if tc.retired {
				req := httptest.NewRequest(http.MethodDelete, "/permit", nil)
				req.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				if w.Code != http.StatusNoContent {
					t.Fatal("could not retire handle")
				}
			}
			if tc.expired {
				r.tunnels[token].accessed = time.Now().Add(-2 * revocationInterval)
			}
			authorization := "Bearer " + token
			if tc.authorization != nil {
				authorization = tc.authorization(token)
			}
			method, path := tc.method, tc.path
			if method == "" {
				method = http.MethodPost
			}
			if path == "" {
				path = "/permit"
			}
			req := httptest.NewRequest(method, path, nil)
			req.Header.Set("Authorization", authorization)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("accepted invalid handle: status %d", w.Code)
			}
			if strings.Contains(w.Body.String(), token) {
				t.Fatal("capability in response")
			}
		})
	}
}

func TestRevocationControlCannotRetargetTunnel(t *testing.T) {
	r := newRevocations(nil)
	cert := revocationCert(t)
	first, err := r.admit(cert, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.admit(cert, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("reused per-tunnel capability")
	}
	binding := r.tunnels[first].key
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		req := httptest.NewRequest(method, "/permit", strings.NewReader(`{"token":"`+second+`","actor":"replacement"}`))
		req.Header.Set("Authorization", "Bearer "+first)
		req.Header.Set("X-Actor-Uid", "replacement")
		req.Header.Set("X-Activation-Id", "later")
		req.Header.Set("X-Tunnel-Handle", second)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if method == http.MethodPost && (w.Code != 200 || r.tunnels[first].key != binding || r.tunnels[second].active) {
			t.Fatal("control request changed binding or activated another tunnel")
		}
		if method == http.MethodDelete && (w.Code != 204 || r.tunnels[first] != nil || r.tunnels[second] == nil) {
			t.Fatal("control request retired another tunnel")
		}
	}
	if permit(r, first).Code != 403 || permit(r, second).Code != 200 {
		t.Fatal("retired handle affected live tunnel")
	}
}

func TestRevocationControlListenAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := &Handler{revocations: newRevocations(nil)}
	done := make(chan error, 1)
	go func() { done <- h.RunRevocations(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 100 * time.Millisecond}
	defer client.CloseIdleConnections()
	for {
		response, err := client.Post("http://127.0.0.1:18081/permit", "text/plain", nil)
		if err == nil {
			response.Body.Close()
			if response.StatusCode != 403 {
				t.Fatal("listener did not reject unknown handle")
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("loopback control listener did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	checked := false
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err != nil || ip.IsLoopback() || ip.To4() == nil || !ip.IsGlobalUnicast() {
			continue
		}
		checked = true
		connection, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), "18081"), 100*time.Millisecond)
		if err == nil {
			connection.Close()
			t.Errorf("control listener exposed on %s", ip)
		}
	}
	if !checked {
		t.Fatal("no non-loopback IPv4 interface to check listener isolation")
	}
}

func TestRevocationLogsExcludeCapabilities(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	ca := newTestCA(t, "actor-ca")
	leaf := ca.issueActorCert(t, "spiffe://substrate-actor.local/worker-for-actor/default/my-actor", actorCertOptions{})
	actor := runningActor()
	h := egressHandler(ca.loadRoots, actor, nil)
	result, err := h.HandleRequestHeaders(context.Background(), egressMetadata(xfccHeader(leaf)))
	if err != nil {
		t.Fatal(err)
	}
	fields := result.DynamicMetadata.GetFields()["dev.ate.egress"].GetStructValue().GetFields()
	token := fields["revocation_token"].GetStringValue()
	if len(token) != 64 {
		t.Fatal("no capability")
	}
	actor.Status.Quarantine = &ateapi.ActorQuarantine{RequestId: "q"}
	r := h.revocations
	r.api = &revocationAPI{get: func(context.Context) (*ateapi.Actor, error) { return actor, nil }}
	r.check(context.Background(), r.tunnels[token].key.actor(), r.tunnels[token].check)
	if permit(r, token).Code != 403 {
		t.Fatal("quarantine did not revoke")
	}
	if !strings.Contains(logs.String(), "connect_revocation_check") {
		t.Fatal("missing revocation event")
	}
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), "Bearer ") {
		t.Fatal("capability leaked in logs")
	}
}

func TestRevocationMixedActivations(t *testing.T) {
	for _, quarantine := range []bool{false, true} {
		t.Run(fmt.Sprint(quarantine), func(t *testing.T) {
			actor := runningActor()
			actor.Status.ActivationId = "activation-2"
			if quarantine {
				actor.Status.Quarantine = &ateapi.ActorQuarantine{RequestId: "q"}
			}
			api := &revocationAPI{get: func(context.Context) (*ateapi.Actor, error) { return actor, nil }}
			r := newRevocations(api)
			plain, err := r.admit(revocationCertWithActivation(t, ""), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			bound, err := r.admit(revocationCert(t), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if len(r.actors) != 1 || r.tunnels[plain].check != r.tunnels[bound].check {
				t.Fatal("mixed tunnels have separate polls")
			}
			r.check(context.Background(), r.tunnels[plain].key.actor(), r.tunnels[plain].check)
			if r.failures.Load() != 1 {
				t.Fatal("activation mismatch missing from failed-check metric")
			}
			if api.calls.Load() != 1 {
				t.Fatal("more than one actor lookup")
			}
			want := 200
			if quarantine {
				want = 403
			}
			if permit(r, plain).Code != want || r.tunnels[plain].key.activation != "" {
				t.Fatal("UID-only tunnel inherited an activation")
			}
			if got := permit(r, bound); got.Code != 403 || strings.TrimSpace(got.Body.String()) != "activation-mismatch" {
				t.Fatal("certified activation did not take precedence")
			}
		})
	}
}

func TestRevocationPollBeforeNewActivationAdmission(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	actor := runningActor()
	actor.Status.ActivationId = "activation-1"
	api := &revocationAPI{get: func(context.Context) (*ateapi.Actor, error) { close(started); <-finish; return actor, nil }}
	r := newRevocations(api)
	old, err := r.admit(revocationCert(t), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	key, group := r.tunnels[old].key.actor(), r.tunnels[old].check
	done := make(chan struct{})
	go func() { defer close(done); r.check(context.Background(), key, group) }()
	<-started
	fresh, err := r.admit(revocationCertWithActivation(t, "activation-2"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	close(finish)
	<-done
	if permit(r, fresh).Code != 200 || r.tunnels[old].check != r.tunnels[fresh].check {
		t.Fatal("older poll revoked fresh admission or split the poll group")
	}
}
