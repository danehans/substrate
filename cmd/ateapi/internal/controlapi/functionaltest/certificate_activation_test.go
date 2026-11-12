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

package functionaltest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/ateletauth/ateletauthtest"
	"go.ate.dev/substrate/cmd/ateapi/internal/workerservice"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/installdefaults"
	"go.ate.dev/substrate/internal/localca"
	"go.ate.dev/substrate/internal/substratex509"
)

// activationMinter mints worker-for-actor certificates for one actor as one
// worker, the way atunnel does through its node's atelet: one key per
// activation, reused on every renewal.
type activationMinter struct {
	t      *testing.T
	server *workerservice.Server
	ctx    context.Context
	worker string
	ref    *ateapi.ObjectRef
	uid    string
	csr    []byte
}

func newActivationMinter(t *testing.T, tc *testContext, node, worker string, ref *ateapi.ObjectRef, uid string) *activationMinter {
	t.Helper()
	ca, err := localca.GenerateCA("1", localca.KeyTypeECDSAP256, 24*time.Hour)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	ateletID := installdefaults.SPIFFEID(installdefaults.SystemNamespace, installdefaults.AteletServiceAccount)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatalf("CreateCertificateRequest: %v", err)
	}
	return &activationMinter{
		t:      t,
		server: workerservice.New(tc.persistence, nil, ateletID, &localca.ConcretePool{CAs: []*localca.CA{ca}, ActiveForSigning: "1"}, "", nil),
		ctx:    ateletauthtest.ContextWith(ateletauthtest.CertOn(t, node)),
		worker: worker,
		ref:    ref,
		uid:    uid,
		csr:    csr,
	}
}

// mint asks for a certificate under activation. On success it returns the
// activation the certificate names.
func (m *activationMinter) mint(activation string) (string, codes.Code) {
	m.t.Helper()
	resp, err := m.server.MintWorkerActorCertificate(m.ctx, &ateapi.MintWorkerActorCertificateRequest{
		Actor:                     m.ref,
		ActorUid:                  m.uid,
		CertificateSigningRequest: m.csr,
		Worker:                    &ateapi.ObjectRef{Name: m.worker},
		ActivationId:              activation,
	})
	if err != nil {
		return "", apierror.Code(err)
	}
	leaf, err := x509.ParseCertificate(resp.GetActorCertificates()[0])
	if err != nil {
		m.t.Fatalf("parse leaf: %v", err)
	}
	identity, err := substratex509.ActorIdentityFromCertificate(leaf)
	if err != nil {
		m.t.Fatalf("ActorIdentityFromCertificate: %v", err)
	}
	if identity.ActorUid != m.uid {
		m.t.Fatalf("certificate ActorUid = %q, want %q", identity.ActorUid, m.uid)
	}
	return identity.ActivationId, codes.OK
}

// mustMint mints under activation, which must succeed and be named by the
// certificate.
func (m *activationMinter) mustMint(stage, activation string) {
	m.t.Helper()
	got, code := m.mint(activation)
	if code != codes.OK {
		m.t.Fatalf("%s: mint under activation %q: code %v, want OK", stage, activation, code)
	}
	if got != activation {
		m.t.Fatalf("%s: certificate ActivationId = %q, want %q", stage, got, activation)
	}
}

// mustRefuse mints under activation, which must be refused with Aborted.
func (m *activationMinter) mustRefuse(stage, activation string) {
	m.t.Helper()
	if _, code := m.mint(activation); code != codes.Aborted {
		m.t.Fatalf("%s: mint under activation %q: code %v, want Aborted", stage, activation, code)
	}
}

// Through the real resume and suspend workflows, a worker mints and renews
// only under the activation it was sent on Run or Restore. It cannot mint for
// an activation the Actor does not hold yet, and once a later resume gives
// the Actor another activation, renewals under the earlier one are refused,
// even though the same worker hosts the Actor again.
func TestActivation_CertificateFollowsActivation(t *testing.T) {
	ns := namespaceForTest("ns-activation-cert")
	tc := setupTest(t, ns)
	defer tc.cleanup()
	ctx := context.Background()

	createTemplate(t, tc, ns)
	worker := createWorkerPod(t, tc, ns, "worker-1", "node1", "pool1")
	ref := &ateapi.ObjectRef{Atespace: testAtespace, Name: "id1"}
	created, err := tc.client.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: ref.GetAtespace(), Name: ref.GetName()},
		ActorTemplate: &ateapi.ObjectRef{Atespace: testAtespace, Name: "tmpl1"},
	}})
	if err != nil {
		t.Fatalf("CreateActor failed: %v", err)
	}
	m := newActivationMinter(t, tc, "node1", worker, ref, created.GetMetadata().GetUid())

	if _, err := tc.client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: ref}); err != nil {
		t.Fatalf("ResumeActor: %v", err)
	}
	first := sentActivation(t, tc)
	mustParseActivation(t, first)

	// The first mint and renewals within the activation.
	for range 3 {
		m.mustMint("first activation", first)
	}
	// An activation the Actor does not hold yet.
	m.mustRefuse("first activation, future ID", ulid.Make().String())
	m.mustRefuse("first activation, no ID", "")

	if _, err := tc.client.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: ref}); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	if _, err := tc.client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: ref}); err != nil {
		t.Fatalf("second ResumeActor: %v", err)
	}
	second := sentActivation(t, tc)
	if second == first {
		t.Fatalf("second resume reused activation %q", first)
	}

	// The first activation's renewal loop is still running on the worker.
	m.mustRefuse("second activation, renewal under the first", first)
	for range 2 {
		m.mustMint("second activation", second)
	}
}

// A resume whose Run or Restore fails leaves the Actor RESUMING with the
// activation it sent. The worker may already be minting under that
// activation. A retried resume replaces it, so the failed attempt's renewals
// are refused and only the retry's activation gets a certificate.
func TestActivation_CertificateRefusedAfterReentry(t *testing.T) {
	ns := namespaceForTest("ns-activation-cert-retry")
	tc := setupTest(t, ns)
	defer tc.cleanup()
	ctx := context.Background()

	createTemplate(t, tc, ns)
	worker := createWorkerPod(t, tc, ns, "worker-1", "node1", "pool1")
	ref := &ateapi.ObjectRef{Atespace: testAtespace, Name: "id1"}
	created, err := tc.client.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: ref.GetAtespace(), Name: ref.GetName()},
		ActorTemplate: &ateapi.ObjectRef{Atespace: testAtespace, Name: "tmpl1"},
	}})
	if err != nil {
		t.Fatalf("CreateActor failed: %v", err)
	}
	m := newActivationMinter(t, tc, "node1", worker, ref, created.GetMetadata().GetUid())

	tc.fakeAtelet.Lock.Lock()
	tc.fakeAtelet.FailRun = status.Error(codes.Unavailable, "worker unreachable")
	tc.fakeAtelet.FailRestore = tc.fakeAtelet.FailRun
	tc.fakeAtelet.Lock.Unlock()
	if _, err := tc.client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: ref}); status.Code(err) != codes.Unavailable {
		t.Fatalf("first ResumeActor = %v, want Unavailable", err)
	}
	failed := sentActivation(t, tc)
	// The worker received Run and minted before the call failed.
	m.mustMint("RESUMING after a failed Run", failed)

	tc.fakeAtelet.Lock.Lock()
	tc.fakeAtelet.FailRun, tc.fakeAtelet.FailRestore = nil, nil
	tc.fakeAtelet.Lock.Unlock()
	if _, err := tc.client.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: ref}); err != nil {
		t.Fatalf("retried ResumeActor: %v", err)
	}
	retried := sentActivation(t, tc)
	if retried == failed {
		t.Fatalf("retry reused activation %q", failed)
	}
	m.mustRefuse("after the retry, renewal under the failed attempt", failed)
	m.mustMint("after the retry", retried)
}
