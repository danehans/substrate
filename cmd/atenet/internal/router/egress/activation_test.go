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
	"context"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"go.ate.dev/substrate/internal/resources"
	"go.ate.dev/substrate/internal/substratex509"
)

// The egress router names the actor from the certificate's SPIFFE URI and does
// not read the ActorIdentity extension, so a certificate whose extension
// carries ActivationId resolves to the same actor, through both the
// agentgateway attribute and XFCC.
func TestActorCertificateWithActivationIdResolvesActor(t *testing.T) {
	ca := newTestCA(t, "actor-identity-ca")
	leaf := ca.issueActorCert(t, "spiffe://substrate-actor.local/worker-for-actor/foo/bar", actorCertOptions{mutate: func(c *x509.Certificate) {
		if err := substratex509.AddActorIdentityToCertificate(&substratex509.ActorIdentity{
			Atespace:     "foo",
			ActorName:    "bar",
			ActorUid:     "3b9f1e77-2c4d-4a80-91be-6d5c8f0a7e21",
			ActivationId: "01JC2Z6N8Q3W9X4Y5Z6A7B8C9D",
		}, c); err != nil {
			t.Fatalf("AddActorIdentityToCertificate: %v", err)
		}
	}})
	identity, err := substratex509.ActorIdentityFromCertificate(leaf)
	if err != nil || identity.ActivationId == "" {
		t.Fatalf("test certificate carries no ActivationId: %+v, %v", identity, err)
	}

	roots, err := ca.loadRoots()
	if err != nil {
		t.Fatalf("loadRoots: %v", err)
	}
	ref, err := verifyActorCertificate([]*x509.Certificate{leaf}, roots)
	if err != nil {
		t.Fatalf("verifyActorCertificate: %v", err)
	}
	if want := (resources.ActorRef{Atespace: "foo", Name: "bar"}); ref != want {
		t.Errorf("verifyActorCertificate = %v, want %v", ref, want)
	}

	h := egressHandler(ca.loadRoots, runningActor(), nil)
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})
	if _, err := h.HandleRequestHeaders(context.Background(), agentgatewayEgressMetadata(string(certificate))); err != nil {
		t.Errorf("HandleRequestHeaders(agentgateway attribute) error = %v, want nil", err)
	}
	if _, err := h.HandleRequestHeaders(context.Background(), egressMetadata(xfccHeader(leaf))); err != nil {
		t.Errorf("HandleRequestHeaders(XFCC) error = %v, want nil", err)
	}
}
