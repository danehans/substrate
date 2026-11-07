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

package networking

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/internal/ateclient"
	"go.ate.dev/substrate/internal/e2e"
	"go.ate.dev/substrate/internal/installdefaults"
	"go.ate.dev/substrate/internal/resources"
)

// TestActorQuarantine quarantines a running egress Actor that has a TLS
// passthrough tunnel open, and checks what the quarantine guide promises:
// the open tunnel closes, ingress fails at once, the Actor can't be resumed,
// reverted or deleted, the gateways can't read its egress policy, only
// quarantine_admin releases it, the release does not resume it, and the
// audit event carries the incident.
func TestActorQuarantine(t *testing.T) {
	ctx := t.Context()
	clients := e2e.GetClients()
	api := clients.SubstrateAPI

	// Its own atespace, so its AccessPolicy touches no other test.
	atespace := fmt.Sprintf("quarantine-%d", time.Now().UnixNano())
	if _, err := api.CreateAtespace(ctx, &ateapi.CreateAtespaceRequest{Atespace: &ateapi.Atespace{Metadata: &ateapi.ResourceMetadata{Name: atespace}}}); err != nil {
		t.Fatalf("CreateAtespace: %v", err)
	}
	t.Cleanup(func() {
		_, _ = api.DeleteAtespace(context.Background(), &ateapi.DeleteAtespaceRequest{Atespace: &ateapi.ObjectRef{Name: atespace}})
	})

	origin, address, rootCA := prepareProtocolOrigin(t, ctx, e2e.ServerPod{
		Name:       "quarantine-origin",
		ImportPath: "go.ate.dev/substrate/internal/e2e/fixtures/testserver",
		Args:       []string{"http"},
		Port:       80,
		TargetPort: 8080,
		TLSPort:    443,
	}, true)

	template := e2e.EgressFixture()
	actorName := fmt.Sprintf("quarantined-%d", time.Now().UnixNano())
	ref := &ateapi.ObjectRef{Atespace: atespace, Name: actorName}
	actorRef := resources.ActorRefFromObjectRef(ref)
	if _, err := api.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: atespace, Name: actorName},
		ActorTemplate: &ateapi.ObjectRef{Atespace: template.Namespace, Name: template.Name},
	}}); err != nil {
		t.Fatalf("CreateActor from %s/%s: %v (deploy the fixture with %s)", template.Namespace, template.Name, err, template.DeployWith)
	}
	e2e.EnsureEgressPolicy(t, ctx, clients, ref, e2e.EgressAllowPassthrough("*"), e2e.EgressAllowHTTP(origin.SvcName()+".cluster.local"))
	resumed, err := e2e.ResumeActorAwaitCapacity(t, ctx, clients, &ateapi.ResumeActorRequest{Actor: ref})
	if err != nil {
		t.Fatalf("ResumeActor: %v", err)
	}
	uid := resumed.GetActor().GetMetadata().GetUid()
	activation := resumed.GetActor().GetStatus().GetActivationId()
	router := mustRouterClient(t, ctx)
	defer router.Close()
	waitForActorRoute(t, ctx, router, actorRef)

	// The demo's /post sends its body 15s after opening the passthrough
	// tunnel, so the tunnel stays open across the quarantine.
	payload, err := json.Marshal(map[string]string{"url": "https://" + address + "/fetch", "rootCA": rootCA})
	if err != nil {
		t.Fatalf("marshaling the /post request: %v", err)
	}
	type result struct {
		status int
		body   string
		err    error
		after  time.Duration
	}
	open := make(chan result, 1)
	since := metav1.NewTime(time.Now().Add(-time.Minute))
	start := time.Now()
	go func() {
		response, err := router.PostJSON(ctx, actorRef, "/post", payload)
		if err != nil {
			open <- result{err: err, after: time.Since(start)}
			return
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		open <- result{status: response.StatusCode, body: string(body), err: err, after: time.Since(start)}
	}()
	assertEgressGatewayConnect(t, ctx, since, atespace, actorName, "443")

	const incident = "INC-E2E-QUARANTINE"
	requestID := "e2e-" + actorName
	quarantined, err := api.QuarantineActor(ctx, &ateapi.QuarantineActorRequest{
		Actor:              ref,
		Reason:             "e2e: exfiltration over a passthrough tunnel",
		IncidentId:         incident,
		ExpectedUid:        uid,
		ExpectedActivation: activation,
		RequestId:          requestID,
	})
	if err != nil {
		t.Fatalf("QuarantineActor: %v", err)
	}
	quarantineTook := time.Since(start)
	receipt := quarantined.GetReceipt()
	wantSteps := []ateapi.QuarantineStep{ateapi.QuarantineStep_QUARANTINE_STEP_HOLD, ateapi.QuarantineStep_QUARANTINE_STEP_MARKED, ateapi.QuarantineStep_QUARANTINE_STEP_SUSPENDED}
	if !slices.Equal(receipt.GetSteps(), wantSteps) || receipt.GetActivationId() != activation || receipt.GetActorUid() != uid ||
		receipt.GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED || receipt.GetHold().GetIncidentId() != incident {
		t.Fatalf("quarantine receipt = %v, want steps %v for activation %s of %s, SUSPENDED, incident %s", receipt, wantSteps, activation, uid, incident)
	}
	t.Logf("quarantine returned %v after the tunnel opened: %v", quarantineTook, receipt.GetSteps())

	// The tunnel the actor opened before the quarantine is closed: the
	// request through it never gets the origin's 200.
	select {
	case got := <-open:
		if got.err == nil && got.status == http.StatusOK && strings.Contains(got.body, `"statusCode":200`) {
			t.Fatalf("the passthrough request open across the quarantine completed after %v: HTTP %d %s", got.after, got.status, got.body)
		}
		t.Logf("the passthrough request open across the quarantine ended after %v: HTTP %d, err %v, body %.200s", got.after, got.status, got.err, got.body)
	case <-time.After(60 * time.Second):
		t.Fatalf("the passthrough request open across the quarantine had not ended 60s after it started")
	}

	// Ingress fails at once instead of parking: the router's resume is
	// refused as a terminal state.
	ingressStart := time.Now()
	response, err := router.Get(ctx, actorRef, "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz on the quarantined Actor through ingress: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	ingressTook := time.Since(ingressStart)
	if response.StatusCode == http.StatusOK {
		t.Fatalf("ingress to the quarantined Actor returned HTTP 200")
	}
	if ingressTook > 5*time.Second {
		t.Errorf("ingress to the quarantined Actor took %v to fail, want it refused without parking", ingressTook)
	}
	t.Logf("ingress to the quarantined Actor: HTTP %d, ate-error-reason %q, after %v", response.StatusCode, response.Header.Get("ate-error-reason"), ingressTook)

	before, err := api.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref})
	if err != nil {
		t.Fatalf("GetActor: %v", err)
	}
	if before.GetStatus().GetQuarantine().GetRequestId() != requestID {
		t.Fatalf("status.quarantine = %v, want request %s", before.GetStatus().GetQuarantine(), requestID)
	}

	_, err = api.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: ref})
	wantQuarantinedRefusal(t, "ResumeActor", err)
	_, err = api.RevertActor(ctx, &ateapi.RevertActorRequest{Actor: ref})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("RevertActor: %v, want FailedPrecondition", err)
	}
	_, err = api.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: ref})
	wantQuarantinedRefusal(t, "DeleteActor", err)
	_, err = api.GetActorEgressPolicy(ctx, &ateapi.GetActorEgressPolicyRequest{Actor: ref, Name: resources.SingletonName})
	wantQuarantinedRefusal(t, "GetActorEgressPolicy", err)
	// Release: neither the bootstrap owner nor a quarantine_manager can, a
	// quarantine_admin can.
	namespace := e2e.CreateNamespace(t).Name
	manager := quarantinePrincipal(t, ctx, clients, namespace, "detector")
	admin := quarantinePrincipal(t, ctx, clients, namespace, "security-admin")
	policy := &ateapi.AccessPolicy{
		Metadata: &ateapi.ResourceMetadata{Name: resources.SingletonName},
		Bindings: []*ateapi.Binding{
			{Role: "quarantine_manager", Members: []string{manager.member}},
			{Role: "quarantine_admin", Members: []string{admin.member}},
			{Role: "editor", Members: []string{admin.member}},
		},
	}
	if _, err := api.CreateAtespaceAccessPolicy(ctx, &ateapi.CreateAtespaceAccessPolicyRequest{Atespace: &ateapi.ObjectRef{Name: atespace}, AccessPolicy: policy}); err != nil {
		t.Fatalf("CreateAtespaceAccessPolicy: %v", err)
	}
	// Wait until the grants are in effect: each role also grants viewer.
	for _, p := range []quarantineTestPrincipal{manager, admin} {
		deadline := time.Now().Add(30 * time.Second)
		for {
			_, err := p.api.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref})
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("GetActor as %s: %v, want the AccessPolicy grant to take effect", p.member, err)
			}
			time.Sleep(time.Second)
		}
	}
	// Read the stored policy after quarantine, including update metadata.
	stored, err := admin.api.GetQuarantinedActorEgressPolicy(ctx, &ateapi.GetQuarantinedActorEgressPolicyRequest{Actor: ref, Name: resources.SingletonName})
	if err != nil {
		t.Fatalf("GetQuarantinedActorEgressPolicy: %v", err)
	}
	// An ordinary policy update is stored and does not lift it.
	stored.Rules = append(stored.Rules, e2e.EgressAllowHTTP("*"))
	if _, err := admin.api.UpdateActorEgressPolicy(ctx, &ateapi.UpdateActorEgressPolicyRequest{Actor: ref, EgressPolicy: stored}); err != nil {
		t.Fatalf("UpdateActorEgressPolicy: %v", err)
	}
	_, err = api.GetActorEgressPolicy(ctx, &ateapi.GetActorEgressPolicyRequest{Actor: ref, Name: resources.SingletonName})
	wantQuarantinedRefusal(t, "GetActorEgressPolicy after a policy update", err)

	// The audit event names the incident, request, caller and steps.
	assertQuarantineAudit(t, ctx, clients, since, incident, requestID, "quarantined")
	assertQuarantineAudit(t, ctx, clients, since, incident, requestID, "suspended")

	release := func(client *ateclient.Client, id string) (*ateapi.ReleaseQuarantineResponse, error) {
		return client.ReleaseQuarantine(ctx, &ateapi.ReleaseQuarantineRequest{
			Actor: ref, QuarantineRequestId: requestID, Reason: "e2e: investigation closed", ExpectedUid: uid, RequestId: id,
		})
	}
	if _, err := release(api, "release-owner"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("ReleaseQuarantine as the bootstrap owner: %v, want PermissionDenied", err)
	}
	if _, err := release(manager.api, "release-manager"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("ReleaseQuarantine as quarantine_manager: %v, want PermissionDenied", err)
	}
	released, err := release(admin.api, "release-admin")
	if err != nil {
		t.Fatalf("ReleaseQuarantine as quarantine_admin: %v", err)
	}
	if !released.GetReceipt().GetTransitioned() || released.GetReceipt().GetCaller() != admin.member {
		t.Fatalf("release receipt = %v, want a transition by %s", released.GetReceipt(), admin.member)
	}
	assertQuarantineAudit(t, ctx, clients, since, incident, requestID, "released")

	after, err := api.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref})
	if err != nil {
		t.Fatalf("GetActor after the release: %v", err)
	}
	if got := after.GetStatus(); got.GetQuarantine() != nil || len(got.GetHolds()) != 0 || got.GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("status after the release = %v, want SUSPENDED with no quarantine or holds", got)
	}

	// Ordinary activity resumes it again, under a new activation.
	waitForActorRoute(t, ctx, router, actorRef)
	resumedAgain, err := api.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref})
	if err != nil {
		t.Fatalf("GetActor after ingress: %v", err)
	}
	if got := resumedAgain.GetStatus().GetActivationId(); got == "" || got == activation {
		t.Errorf("activation after the release = %q, want a new one (quarantined %s)", got, activation)
	}
	if _, err := api.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: ref}); err != nil {
		t.Logf("cleanup: SuspendActor: %v", err)
	}
	if _, err := api.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: ref}); err != nil {
		t.Logf("cleanup: DeleteActor: %v", err)
	}
}

// wantQuarantinedRefusal fails unless err is FailedPrecondition with the
// quarantine's ErrorInfo.
func wantQuarantinedRefusal(t *testing.T, op string, err error) {
	t.Helper()
	s, _ := status.FromError(err)
	if s.Code() != codes.FailedPrecondition {
		t.Fatalf("%s: %v, want FailedPrecondition", op, err)
	}
	for _, detail := range s.Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok && info.GetReason() == "TERMINAL_STATE" && info.GetMetadata()["cause"] == "ACTOR_QUARANTINED" {
			return
		}
	}
	t.Fatalf("%s: details %v, want ErrorInfo TERMINAL_STATE with cause ACTOR_QUARANTINED", op, s.Details())
}

type quarantineTestPrincipal struct {
	member string
	api    *ateclient.Client
}

// quarantinePrincipal creates a ServiceAccount and returns a client that
// authenticates as it.
func quarantinePrincipal(t *testing.T, ctx context.Context, clients *e2e.Clients, namespace, name string) quarantineTestPrincipal {
	t.Helper()
	if _, err := clients.K8s.CoreV1().ServiceAccounts(namespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating ServiceAccount %s/%s: %v", namespace, name, err)
	}
	expiration := int64(3600)
	token, err := clients.K8s.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, name, &authv1.TokenRequest{Spec: authv1.TokenRequestSpec{
		Audiences:         []string{installdefaults.APIServiceName + "." + installdefaults.SystemNamespace + ".svc"},
		ExpirationSeconds: &expiration,
	}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("minting a token for %s/%s: %v", namespace, name, err)
	}
	tokenFile := t.TempDir() + "/token"
	if err := os.WriteFile(tokenFile, []byte(token.Status.Token), 0o600); err != nil {
		t.Fatalf("writing token: %v", err)
	}
	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, err := ateclient.NewClient(connectCtx, e2e.KubeConfig, e2e.KubeContext, "", tokenFile, false)
	if err != nil {
		t.Fatalf("ateclient.NewClient: %v", err)
	}
	t.Cleanup(client.Close)
	return quarantineTestPrincipal{member: fmt.Sprintf("user:system:serviceaccount:%s:%s", namespace, name), api: client}
}

// assertQuarantineAudit waits for the ate.actor.quarantine_changed record of
// action for requestID with incident. An install that exports actor events
// with OTEL_LOGS_EXPORTER=otlp,console, as the kustomize kind install does,
// writes it to ate-api-server's stdout. One that exports otlp only, as the
// Helm chart does, sends it to the collector, whose debug exporter prints it.
func assertQuarantineAudit(t *testing.T, ctx context.Context, clients *e2e.Clients, since metav1.Time, incident, requestID, action string) {
	t.Helper()
	namespace := e2e.SystemNamespace()
	deployment, err := clients.K8s.AppsV1().Deployments(namespace).Get(ctx, e2e.ResourceName("ate-api-server"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting the ate-api-server deployment: %v", err)
	}
	apiServer := labels.SelectorFromSet(deployment.Spec.Selector.MatchLabels).String()
	// stdout holds one JSON record per line.
	inStdout := func(logs string) bool {
		for line := range strings.SplitSeq(logs, "\n") {
			if strings.Contains(line, "Actor quarantine changed") &&
				strings.Contains(line, `"ate.actor.quarantine.action":"`+action+`"`) &&
				strings.Contains(line, `"ate.actor.quarantine.request_id":"`+requestID+`"`) &&
				strings.Contains(line, `"ate.actor.quarantine.incident_id":"`+incident+`"`) {
				return true
			}
		}
		return false
	}
	// The debug exporter prints each record's attributes on lines of their
	// own, under a "LogRecord #" header.
	inCollector := func(logs string) bool {
		for _, record := range strings.Split(logs, "LogRecord #") {
			if strings.Contains(record, "ate.actor.quarantine_changed") &&
				strings.Contains(record, "ate.actor.quarantine.action: Str("+action+")") &&
				strings.Contains(record, "ate.actor.quarantine.request_id: Str("+requestID+")") &&
				strings.Contains(record, "ate.actor.quarantine.incident_id: Str("+incident+")") {
				return true
			}
		}
		return false
	}
	sources := []struct {
		namespace, selector string
		found               func(string) bool
	}{
		{namespace, apiServer, inStdout},
		{"otel-system", "app=opentelemetry-collector", inCollector},
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		for _, source := range sources {
			pods, err := clients.K8s.CoreV1().Pods(source.namespace).List(ctx, metav1.ListOptions{LabelSelector: source.selector})
			if err != nil {
				t.Fatalf("listing pods %s in %s: %v", source.selector, source.namespace, err)
			}
			for _, pod := range pods.Items {
				logs, err := clients.K8s.CoreV1().Pods(source.namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: pod.Spec.Containers[0].Name, SinceTime: &since}).DoRaw(ctx)
				if err != nil {
					t.Fatalf("reading logs of %s/%s: %v", source.namespace, pod.Name, err)
				}
				if source.found(string(logs)) {
					t.Logf("audit record %s for %s found in %s/%s", action, requestID, source.namespace, pod.Name)
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ate.actor.quarantine_changed %s record for %s with incident %s in ate-api-server's stdout or the collector's debug output", action, requestID, incident)
		}
		time.Sleep(time.Second)
	}
}
