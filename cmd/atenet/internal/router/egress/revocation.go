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

// Package egress implements the ext_proc handler for outbound actor traffic.
// It authenticates the actor behind an egress CONNECT and authorizes what goes
// through the tunnel against the actor's EgressPolicy. The dataplane decides
// TLS at the ClientHello using the SNI rules returned on CONNECT.
//
// Identity comes from the actor certificate presented in the mTLS handshake,
// never from a request header. On the inner legs it arrives as filter state
// Envoy derived from that certificate, which nothing inside the tunnel can
// write. The filter chain name tells the handler which leg it is on.
package egress

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/internal/substratex509"
)

const revocationControlAddress = "127.0.0.1:18081"

const revocationInterval = 5 * time.Second
const revocationRPCDeadline = 2 * time.Second
const revocationFreshness = revocationInterval + revocationRPCDeadline

// executionKey is frozen from the verified CONNECT certificate.
type executionKey struct{ atespace, name, uid, activation string }
type actorKey struct{ atespace, name, uid string }

func (key executionKey) actor() actorKey { return actorKey{key.atespace, key.name, key.uid} }

type tunnelPermit struct {
	key      executionKey
	accessed time.Time
	admitted time.Time
	active   bool
	check    *actorCheck
	phases   map[string]bool
	reason   string
}
type actorCheck struct {
	checked, due time.Time
	busy         bool
	tunnels      map[*tunnelPermit]struct{}
	reason       string
}

// revocations holds local capabilities, never identities supplied by a control request.
type revocations struct {
	mu                         sync.Mutex
	api                        ateapi.ControlClient
	tunnels                    map[string]*tunnelPermit
	actors                     map[actorKey]*actorCheck
	lookups, failures, overdue atomic.Int64
}

func newRevocations(api ateapi.ControlClient) *revocations {
	return &revocations{api: api, tunnels: make(map[string]*tunnelPermit), actors: make(map[actorKey]*actorCheck)}
}
func (r *revocations) admit(cert certifiedActor, checked time.Time) (string, error) {
	id, err := substratex509.ActorIdentityFromCertificate(cert.leaf)
	if err != nil || id == nil || id.ActorUid == "" {
		return "", fmt.Errorf("revocation requires a certified actor UID")
	}
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(secret[:])
	now := time.Now()
	if now.Sub(checked) >= revocationFreshness {
		return "", fmt.Errorf("actor check expired during CONNECT admission")
	}
	key := executionKey{id.Atespace, id.ActorName, id.ActorUid, id.ActivationId}
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.actors[key.actor()]
	if a == nil || a.reason != "" || !now.Before(a.checked.Add(revocationFreshness)) {
		delay := time.Duration((int(secret[0])<<8|int(secret[1]))%4900) * time.Millisecond
		a = &actorCheck{checked: checked, due: checked.Add(delay), tunnels: make(map[*tunnelPermit]struct{})}
		r.actors[key.actor()] = a
	}
	tunnel := &tunnelPermit{key: key, accessed: now, admitted: checked, check: a}
	r.tunnels[token] = tunnel
	a.tunnels[tunnel] = struct{}{}
	return token, nil
}

// retire removes a capability from both indexes while mu is held.
func (r *revocations) retire(token string) {
	if tunnel := r.tunnels[token]; tunnel != nil {
		delete(tunnel.check.tunnels, tunnel)
	}
	delete(r.tunnels, token)
}

// ServeHTTP authenticates possession of a per-CONNECT capability. It cannot
// register an actor or change a capability's execution binding.
func (r *revocations) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	token, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !ok || len(token) != 64 {
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	phase := req.Header.Get("X-Permit-Phase")
	if phase == "" {
		phase = "network"
	}
	if phase != "network" && phase != "connect" {
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.tunnels[token]
	if t == nil || now.Sub(t.accessed) >= 2*revocationInterval {
		r.retire(token)
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	if req.URL.Path != "/permit" || req.URL.RawQuery != "" {
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	if req.Method == http.MethodDelete {
		delete(t.phases, phase)
		if len(t.phases) == 0 {
			r.retire(token)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if req.Method != http.MethodPost {
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	a := t.check
	if t.phases == nil {
		t.phases = make(map[string]bool)
	}
	t.phases[phase] = true
	t.active = true
	t.accessed = now
	if t.reason != "" || a.reason != "" || !now.Before(a.checked.Add(revocationFreshness)) {
		reason := t.reason
		if reason == "" {
			reason = a.reason
		}
		if reason == "" {
			reason = "actor-check-overdue"
			a.reason = reason
		}
		http.Error(w, reason, http.StatusForbidden)
		return
	}
	// The caller starts its watchdog before sending the request, so transit
	// time cannot extend this remaining freshness budget.
	fmt.Fprint(w, a.checked.Add(revocationFreshness).Sub(now).Milliseconds())
}

func (r *revocations) run(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	slots := make(chan struct{}, 32)
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			r.mu.Lock()
			live := make(map[actorKey]bool)
			present := make(map[actorKey]bool)
			for token, t := range r.tunnels {
				if now.Sub(t.accessed) >= 2*revocationInterval {
					r.retire(token)
					continue
				}
				present[t.key.actor()] = true
				if t.active && t.reason == "" {
					if r.actors[t.key.actor()] == t.check {
						live[t.key.actor()] = true
					}
				}
			}
			for key, a := range r.actors {
				if !live[key] {
					if present[key] {
						continue
					}
					delete(r.actors, key)
					continue
				}
				if a.reason != "" {
					continue
				}
				if !now.Before(a.checked.Add(revocationFreshness)) {
					a.reason = "actor-check-overdue"
					r.overdue.Add(1)
					continue
				}
				if a.busy || now.Before(a.due) {
					continue
				}
				select {
				case slots <- struct{}{}:
					a.busy = true
					go func(key actorKey, a *actorCheck) { defer func() { <-slots }(); r.check(ctx, key, a) }(key, a)
				default:
					// Backlog never moves a freshness deadline.
				}
			}
			r.mu.Unlock()
		}
	}
}
func (r *revocations) check(ctx context.Context, key actorKey, a *actorCheck) {
	started := time.Now()
	r.lookups.Add(1)
	ctx, cancel := context.WithTimeout(ctx, revocationRPCDeadline)
	defer cancel()
	actor, err := r.api.GetActor(ctx, &ateapi.GetActorRequest{Actor: &ateapi.ObjectRef{Atespace: key.atespace, Name: key.name}})
	reason := ""
	switch {
	case err != nil || ctx.Err() != nil || time.Since(started) >= revocationRPCDeadline:
		reason = "actor-check-failed"
	case actor.GetMetadata().GetUid() != key.uid:
		reason = "actor-uid-mismatch"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.actors[key] != a || a.reason != "" {
		return
	}
	a.busy = false
	if !time.Now().Before(a.checked.Add(revocationFreshness)) {
		reason = "actor-check-overdue"
	}
	failed := reason != ""
	for tunnel := range a.tunnels {
		if tunnel.reason != "" {
			continue
		}
		tunnel.reason = reason
		if tunnel.reason == "" && tunnel.key.activation != "" && !tunnel.admitted.After(started) && tunnel.key.activation != actor.GetStatus().GetActivationId() {
			tunnel.reason = "activation-mismatch"
		}
		if tunnel.reason == "" && actor.GetStatus().GetQuarantine() != nil {
			tunnel.reason = "actor-quarantined"
		}
		if tunnel.reason != "" {
			failed = true
			slog.WarnContext(ctx, "egress tunnel revoked", "target", "connect_revocation_check", "reason", tunnel.reason, "atespace", tunnel.key.atespace, "actor", tunnel.key.name, "actor_uid", tunnel.key.uid, "activation_id", tunnel.key.activation)
		}
	}
	if failed {
		r.failures.Add(1)
	}
	if reason == "" && actor.GetStatus().GetQuarantine() != nil {
		reason = "actor-quarantined"
	}
	if reason != "" {
		a.reason = reason
		if reason == "actor-check-overdue" {
			r.overdue.Add(1)
		}
		return
	}
	// Use RPC start, rather than completion, as the freshness origin.
	a.checked = started
	var jitter [1]byte
	if _, err := rand.Read(jitter[:]); err != nil {
		a.reason = "actor-check-failed"
		return
	}
	a.due = started.Add(revocationInterval - 100*time.Millisecond - time.Duration(jitter[0])*time.Millisecond)
}

// RunRevocations serves only pod-local capability-authenticated control calls.
func (h *Handler) RunRevocations(ctx context.Context) error {
	registration, err := h.revocations.registerMetrics()
	if err != nil {
		return err
	}
	defer func() {
		if err := registration.Unregister(); err != nil {
			slog.WarnContext(ctx, "unregister revocation metrics", "error", err)
		}
	}()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	listener, err := net.Listen("tcp", revocationControlAddress)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: h.revocations, ReadHeaderTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, MaxHeaderBytes: 4096}
	done := make(chan struct{})
	go func() { defer close(done); h.revocations.run(ctx) }()
	defer func() { cancel(); <-done }()
	go func() { <-ctx.Done(); server.Close() }()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (r *revocations) registerMetrics() (metric.Registration, error) {
	meter := otel.Meter("atenet-router")
	actors, err := meter.Int64ObservableGauge("atenet.router.revocation.actors", metric.WithUnit("{actor}"))
	if err != nil {
		return nil, err
	}
	tunnels, err := meter.Int64ObservableGauge("atenet.router.revocation.tunnels", metric.WithUnit("{tunnel}"))
	if err != nil {
		return nil, err
	}
	lookups, err := meter.Int64ObservableCounter("atenet.router.revocation.lookups", metric.WithUnit("{lookup}"))
	if err != nil {
		return nil, err
	}
	failures, err := meter.Int64ObservableCounter("atenet.router.revocation.failures", metric.WithUnit("{check}"))
	if err != nil {
		return nil, err
	}
	overdue, err := meter.Int64ObservableCounter("atenet.router.revocation.overdue", metric.WithUnit("{actor}"))
	if err != nil {
		return nil, err
	}
	return meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		active := map[actorKey]bool{}
		var count int64
		for _, t := range r.tunnels {
			if t.active {
				active[t.key.actor()] = true
				count++
			}
		}
		o.ObserveInt64(actors, int64(len(active)))
		o.ObserveInt64(tunnels, count)
		o.ObserveInt64(lookups, r.lookups.Load())
		o.ObserveInt64(failures, r.failures.Load())
		o.ObserveInt64(overdue, r.overdue.Load())
		return nil
	}, actors, tunnels, lookups, failures, overdue)
}
