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

package substratex509

import (
	"crypto/x509"
	"reflect"
	"strings"
	"testing"
)

const testActivationID = "01JC2Z6N8Q3W9X4Y5Z6A7B8C9D"

// actorExtensionValue returns the raw ActorIdentity extension value of cert.
func actorExtensionValue(t *testing.T, cert *x509.Certificate) string {
	t.Helper()
	for _, ext := range cert.Extensions {
		if ext.Id.Equal(testActorIdentityOID) {
			return string(ext.Value)
		}
	}
	t.Fatal("certificate has no ActorIdentity extension")
	return ""
}

// The extension is JSON with PascalCase keys. ActivationId is one more key,
// and is omitted when empty, so a certificate for an Actor with no activation
// encodes exactly as before the field existed.
func TestActorIdentityActivationIdEncoding(t *testing.T) {
	for _, tc := range []struct {
		name  string
		actor ActorIdentity
		want  string
	}{
		{
			name:  "no activation",
			actor: ActorIdentity{Atespace: "team-a", ActorName: "researcher", ActorUid: "actor-uid"},
			want:  `{"Atespace":"team-a","ActorName":"researcher","ActorUid":"actor-uid"}`,
		},
		{
			name:  "activation",
			actor: ActorIdentity{Atespace: "team-a", ActorName: "researcher", ActorUid: "actor-uid", ActivationId: testActivationID},
			want:  `{"Atespace":"team-a","ActorName":"researcher","ActorUid":"actor-uid","ActivationId":"` + testActivationID + `"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			template := &x509.Certificate{}
			if err := AddActorIdentityToCertificate(&tc.actor, template); err != nil {
				t.Fatalf("AddActorIdentityToCertificate: %v", err)
			}
			cert := mintCert(t, template)
			if got := actorExtensionValue(t, cert); got != tc.want {
				t.Errorf("extension value = %s, want %s", got, tc.want)
			}
			got, err := ActorIdentityFromCertificate(cert)
			if err != nil {
				t.Fatalf("ActorIdentityFromCertificate: %v", err)
			}
			if !reflect.DeepEqual(*got, tc.actor) {
				t.Errorf("ActorIdentityFromCertificate = %+v, want %+v", *got, tc.actor)
			}
		})
	}
}

func TestActorIdentityActivationIdBound(t *testing.T) {
	long := ActorIdentity{Atespace: "team-a", ActorName: "researcher", ActorUid: "actor-uid", ActivationId: testActivationID + "X"}
	if err := AddActorIdentityToCertificate(&long, &x509.Certificate{}); err == nil || !strings.Contains(err.Error(), "ActivationId") {
		t.Errorf("AddActorIdentityToCertificate with a 27-byte ActivationId: error = %v, want it refused", err)
	}
	cert := mintCertWithActorExtension(t, []byte(`{"Atespace":"team-a","ActorName":"researcher","ActorUid":"actor-uid","ActivationId":"`+long.ActivationId+`"}`))
	if _, err := ActorIdentityFromCertificate(cert); err == nil || !strings.Contains(err.Error(), "ActivationId") {
		t.Errorf("ActorIdentityFromCertificate with a 27-byte ActivationId: error = %v, want it refused", err)
	}
}
