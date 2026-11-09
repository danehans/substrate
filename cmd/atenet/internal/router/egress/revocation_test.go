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
	"context"
	"encoding/pem"
	"fmt"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/internal/resources"
	"google.golang.org/grpc"
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
	t.Helper()
	ca := newTestCA(t, "revocation-ca")
	return certifiedActor{ref: resources.ActorRef{Atespace: testEgressAtespace, Name: testEgressActor}, leaf: ca.issueActorCert(t, "spiffe://substrate-actor.local/worker-for-actor/default/my-actor", actorCertOptions{activation: "activation-1"})}
}
func permit(r *revocations, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/permit", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
func TestRevocationChecks(t *testing.T) {
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
		t.Run(tc.name, func(t *testing.T) {
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
				token, err := r.admit(revocationCert(t), time.Now())
				if err != nil {
					t.Fatal(err)
				}
				if got := permit(r, token).Code; got != 200 {
					t.Fatalf("initial permit: %d", got)
				}
				group := r.tunnels[token].check
				r.check(context.Background(), r.tunnels[token].key, group)
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
		cert := revocationCert(t)
		tokens := make([]string, 10)
		for i := range tokens {
			var err error
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
		r.check(context.Background(), r.tunnels[token].key, r.tunnels[token].check)
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
			if activation == "" {
				if fields["revocation_mode"].GetStringValue() != "legacy" || fields["revocation_token"] != nil {
					t.Fatal("legacy certificate behavior changed")
				}
				return
			}
			token := fields["revocation_token"].GetStringValue()
			if fields["revocation_mode"].GetStringValue() != "required" || len(token) != 64 {
				t.Fatal("missing exact-run permit")
			}
			binding := fields["revocation_identity"].GetStringValue()
			if !strings.Contains(binding, testEgressActorUID) || !strings.Contains(binding, activation) {
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
				group := &actorCheck{checked: checked, due: checked.Add(time.Duration((i*17+replica*97)%4900) * time.Millisecond)}
				r.actors[key] = group
				for tunnel := range tunnels {
					token := fmt.Sprintf("%064x", i*tunnels+tunnel)
					r.tunnels[token] = &tunnelPermit{key: key, accessed: checked, active: true, check: group}
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
			r.actors[key] = group
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
