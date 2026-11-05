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

package workerservice

import (
	"crypto/x509"
	"strings"
	"testing"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/ateletauth/ateletauthtest"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/substratex509"
	"google.golang.org/grpc/codes"
)

const (
	activationA = "01JC2Z6N8Q3W9X4Y5Z6A7B8C9D"
	activationB = "01JC2Z7P0R1S2T3V4W5X6Y7Z8A"
)

// mintActivationRequest asks for the certificate of the actor that
// newFakeStoreWithActor seeds, as worker-1, under activation.
func mintActivationRequest(actor *ateapi.Actor, csr []byte, activation string) *ateapi.MintWorkerActorCertificateRequest {
	return &ateapi.MintWorkerActorCertificateRequest{
		Actor:                     &ateapi.ObjectRef{Atespace: actor.GetMetadata().GetAtespace(), Name: actor.GetMetadata().GetName()},
		ActorUid:                  actor.GetMetadata().GetUid(),
		CertificateSigningRequest: csr,
		Worker:                    &ateapi.ObjectRef{Name: "worker-1"},
		ActivationId:              activation,
	}
}

// leafActorIdentity parses the ActorIdentity of the leaf of a mint response.
func leafActorIdentity(t *testing.T, resp *ateapi.MintWorkerActorCertificateResponse) (*x509.Certificate, *substratex509.ActorIdentity) {
	t.Helper()
	leaf, err := x509.ParseCertificate(resp.GetActorCertificates()[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	identity, err := substratex509.ActorIdentityFromCertificate(leaf)
	if err != nil {
		t.Fatalf("ActorIdentityFromCertificate: %v", err)
	}
	return leaf, identity
}

// The mint issues only for the activation the Actor holds, and the
// certificate names it.
func TestMintWorkerActorCertificate_Activation(t *testing.T) {
	st, actor := newFakeStoreWithActor("team-a", "my-actor", "3b9f1e77-2c4d-4a80-91be-6d5c8f0a7e21")
	s := New(st, &fakeControl{}, testAteletSPIFFEID, newTestCAPool(t), "", nil)
	csr, _ := generateTestCSR(t)
	ctx := ateletauthtest.ContextWith(ateletauthtest.CertOn(t, "node-1"))

	for _, tc := range []struct {
		name      string
		current   string
		requested string
		want      codes.Code
	}{
		{name: "current activation", current: activationA, requested: activationA, want: codes.OK},
		{name: "no activation on either side", current: "", requested: "", want: codes.OK},
		{
			// The Actor has moved on to B: the worker's renewal for A is
			// refused rather than reissued under B.
			name: "earlier activation", current: activationB, requested: activationA, want: codes.Aborted,
		},
		{
			// A worker cannot obtain a certificate for an activation the
			// Actor does not hold yet.
			name: "later activation", current: activationA, requested: activationB, want: codes.Aborted,
		},
		{
			// A worker that omits the activation is refused once the Actor
			// has one.
			name: "missing activation", current: activationA, requested: "", want: codes.Aborted,
		},
		{
			// An Actor without an activation (a record written by a control
			// plane without this patch) matches only an empty request.
			name: "activation for an actor with none", current: "", requested: activationA, want: codes.Aborted,
		},
		{name: "overlong activation", current: activationA, requested: activationA + "X", want: codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actor.Status.ActivationId = tc.current
			resp, err := s.MintWorkerActorCertificate(ctx, mintActivationRequest(actor, csr, tc.requested))
			if got := apierror.Code(err); got != tc.want {
				t.Fatalf("MintWorkerActorCertificate() code = %v, want %v (err: %v)", got, tc.want, err)
			}
			if tc.want != codes.OK {
				if tc.want == codes.Aborted && !strings.Contains(err.Error(), "activation") {
					t.Errorf("refusal %q does not name the activation", err)
				}
				return
			}
			_, identity := leafActorIdentity(t, resp)
			if identity.ActivationId != tc.current {
				t.Errorf("certificate ActivationId = %q, want %q", identity.ActivationId, tc.current)
			}
		})
	}
}

// A worker renews its certificate several times within one activation, with
// the key it generated for that activation. Every renewal is issued and names
// the same activation. Once the Actor holds a later activation, the next
// renewal under the earlier one is refused with Aborted, which ends atunnel's
// renewal loop.
func TestMintWorkerActorCertificate_RenewalWithinActivation(t *testing.T) {
	st, actor := newFakeStoreWithActor("team-a", "my-actor", "3b9f1e77-2c4d-4a80-91be-6d5c8f0a7e21")
	s := New(st, &fakeControl{}, testAteletSPIFFEID, newTestCAPool(t), "", nil)
	csr, key := generateTestCSR(t)
	ctx := ateletauthtest.ContextWith(ateletauthtest.CertOn(t, "node-1"))
	actor.Status.ActivationId = activationA

	var serials []string
	for i := range 3 {
		resp, err := s.MintWorkerActorCertificate(ctx, mintActivationRequest(actor, csr, activationA))
		if err != nil {
			t.Fatalf("renewal %d: %v", i, err)
		}
		leaf, identity := leafActorIdentity(t, resp)
		if !key.PublicKey.Equal(leaf.PublicKey) {
			t.Errorf("renewal %d: leaf key does not match the activation's key", i)
		}
		if identity.ActivationId != activationA {
			t.Errorf("renewal %d: ActivationId = %q, want %q", i, identity.ActivationId, activationA)
		}
		serials = append(serials, leaf.SerialNumber.String())
	}
	if serials[0] == serials[1] || serials[1] == serials[2] {
		t.Errorf("renewals reissued the same certificate: serials %v", serials)
	}

	// The same worker now hosts a later activation of the Actor.
	actor.Status.ActivationId = activationB
	_, err := s.MintWorkerActorCertificate(ctx, mintActivationRequest(actor, csr, activationA))
	if got := apierror.Code(err); got != codes.Aborted {
		t.Fatalf("renewal under the earlier activation: code %v, want Aborted (err: %v)", got, err)
	}
}
