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
	"errors"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/internal/resources"
)

const (
	detector      = "spiffe://cluster.local/ns/sec/sa/detector"
	securityAdmin = "spiffe://cluster.local/ns/sec/sa/admin"
)

var (
	stepsPlaced     = []ateapi.QuarantineStep{ateapi.QuarantineStep_QUARANTINE_STEP_HOLD, ateapi.QuarantineStep_QUARANTINE_STEP_MARKED}
	stepsSuspended  = []ateapi.QuarantineStep{ateapi.QuarantineStep_QUARANTINE_STEP_HOLD, ateapi.QuarantineStep_QUARANTINE_STEP_MARKED, ateapi.QuarantineStep_QUARANTINE_STEP_SUSPENDED}
	stepsTerminated = []ateapi.QuarantineStep{ateapi.QuarantineStep_QUARANTINE_STEP_HOLD, ateapi.QuarantineStep_QUARANTINE_STEP_MARKED, ateapi.QuarantineStep_QUARANTINE_STEP_TERMINATED}
)

func (f *receiptFixture) quarantine(principal string, req *ateapi.QuarantineActorRequest) (*ateapi.QuarantineActorResponse, error) {
	req.Actor = f.ref
	if req.Reason == "" {
		req.Reason = "credential exfiltration"
	}
	if req.IncidentId == "" {
		req.IncidentId = "INC-7"
	}
	return f.tc.client.QuarantineActor(asPrincipal(f.ctx, principal), req)
}

func (f *receiptFixture) mustQuarantine(req *ateapi.QuarantineActorRequest) *ateapi.QuarantineActorResponse {
	f.t.Helper()
	resp, err := f.quarantine(detector, req)
	if err != nil {
		f.t.Fatalf("QuarantineActor(%s): %v", req.GetRequestId(), err)
	}
	return resp
}

func (f *receiptFixture) releaseQuarantine(principal, quarantineRequestID, requestID string) (*ateapi.ReleaseQuarantineResponse, error) {
	return f.tc.client.ReleaseQuarantine(asPrincipal(f.ctx, principal), &ateapi.ReleaseQuarantineRequest{
		Actor:               f.ref,
		QuarantineRequestId: quarantineRequestID,
		Reason:              "investigation closed",
		RequestId:           requestID,
	})
}

// restores reports whether atelet was asked to restore since the last call,
// and clears it.
func (f *receiptFixture) restores() bool {
	a := f.tc.fakeAtelet
	a.Lock.Lock()
	defer a.Lock.Unlock()
	called := a.RestoreRequest != nil
	a.RestoreRequest = nil
	return called
}

// wantQuarantined fails unless err is the refusal of a quarantined Actor,
// with the ErrorInfo agentgateway's ingress and atunnel stop on.
func wantQuarantined(t *testing.T, op string, err error) {
	t.Helper()
	s, _ := status.FromError(err)
	if s.Code() != codes.FailedPrecondition {
		t.Fatalf("%s: error = %v, want FailedPrecondition", op, err)
	}
	want := &errdetails.ErrorInfo{Domain: "ate.dev", Reason: "TERMINAL_STATE", Metadata: map[string]string{"cause": "ACTOR_QUARANTINED"}}
	details := s.Details()
	if len(details) != 1 {
		t.Fatalf("%s: details = %v, want one ErrorInfo", op, details)
	}
	if got, ok := details[0].(*errdetails.ErrorInfo); !ok || !proto.Equal(got, want) {
		t.Fatalf("%s: details[0] = %v, want %v", op, details[0], want)
	}
}

// A quarantine conditioned on an activation the Actor no longer holds is
// refused and changes nothing: no hold, no quarantine, no suspend, no
// receipt.
func TestActorQuarantine_StaleActivation(t *testing.T) {
	f := newReceiptFixture(t, "ns-quarantine-stale")
	stale := f.resume()
	if _, err := f.suspend(&ateapi.SuspendActorRequest{}); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	f.resume()
	f.checkpoints()
	before := f.get()

	_, err := f.quarantine(detector, &ateapi.QuarantineActorRequest{ExpectedActivation: stale, RequestId: "q-stale"})
	wantCode(t, "QuarantineActor for the earlier activation", err, codes.FailedPrecondition)
	f.wantUnchanged(before)
	if f.checkpoints() {
		t.Errorf("a refused quarantine checkpointed the Actor")
	}
	if f.tc.fakeAtelet.TerminateCalled {
		t.Errorf("a refused quarantine terminated the Actor")
	}
	actorRef := resources.ActorRefFromObjectRef(f.ref)
	for _, id := range []string{"q-stale", "q-stale/suspend"} {
		if _, err := f.tc.persistence.GetActorLifecycleReceipt(f.ctx, actorRef, id); err == nil {
			t.Errorf("a refused quarantine stored receipt %q", id)
		}
	}
	// The current activation can still be quarantined.
	if got := f.mustQuarantine(&ateapi.QuarantineActorRequest{ExpectedActivation: before.GetStatus().GetActivationId(), RequestId: "q-current"}).GetReceipt().GetSteps(); !cmp.Equal(got, stepsSuspended) {
		t.Errorf("steps = %v, want %v", got, stepsSuspended)
	}
}

// Quarantining a RUNNING Actor holds it, marks it and suspends the
// activation it found. The receipt records the call and each step.
func TestActorQuarantine_SuspendsRunningActor(t *testing.T) {
	f := newReceiptFixture(t, "ns-quarantine-running")
	activation := f.resume()
	uid := f.get().GetMetadata().GetUid()
	f.checkpoints()

	resp := f.mustQuarantine(&ateapi.QuarantineActorRequest{ExpectedUid: uid, ExpectedActivation: activation, RequestId: "q-1"})
	if !f.checkpoints() {
		t.Errorf("atelet was not asked to checkpoint the quarantined activation")
	}
	r := resp.GetReceipt()
	want := &ateapi.LifecycleReceipt{
		RequestId:          "q-1",
		Operation:          ateapi.LifecycleOperation_LIFECYCLE_OPERATION_QUARANTINE,
		Actor:              f.ref,
		ActorUid:           uid,
		ExpectedUid:        uid,
		ExpectedActivation: activation,
		ActivationId:       activation,
		Transitioned:       true,
		State:              ateapi.ActorState_ACTOR_STATE_SUSPENDED,
		Mode:               ateapi.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS,
		Caller:             "user:" + detector,
		Reason:             "credential exfiltration",
		Steps:              stepsSuspended,
		SuspendRequestId:   "q-1/suspend",
		Hold: &ateapi.ActorHold{
			Holder: "quarantine", Reason: "credential exfiltration", IncidentId: "INC-7", RequestId: "q-1",
			ExpectedUid: uid, ExpectedActivation: activation,
		},
		Quarantine: &ateapi.ActorQuarantine{RequestId: "q-1", Caller: "user:" + detector, Reason: "credential exfiltration", IncidentId: "INC-7", ActivationId: activation},
	}
	ignore := protocmp.IgnoreFields(&ateapi.LifecycleReceipt{}, "create_time", "expire_time", "snapshot_durable")
	ignoreTimes := protocmp.IgnoreFields(&ateapi.ActorHold{}, "create_time")
	if diff := cmp.Diff(want, r, protocmp.Transform(), ignore, ignoreTimes); diff != "" {
		t.Errorf("receipt (-want +got):\n%s", diff)
	}

	actor := f.get()
	if got := actor.GetStatus().GetState(); got != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		t.Errorf("state = %v, want SUSPENDED", got)
	}
	if got, want := holdersOf(actor), []string{"quarantine/q-1"}; !cmp.Equal(got, want) {
		t.Errorf("holds = %v, want %v", got, want)
	}
	if got := actor.GetStatus().GetQuarantine(); got.GetRequestId() != "q-1" || got.GetActivationId() != activation || got.GetCreateTime() == nil {
		t.Errorf("status.quarantine = %v, want q-1 at activation %s", got, activation)
	}
	// The suspend's own receipt names the activation it stopped.
	suspend, err := f.tc.persistence.GetActorLifecycleReceipt(f.ctx, resources.ActorRefFromObjectRef(f.ref), "q-1/suspend")
	if err != nil {
		t.Fatalf("suspend receipt: %v", err)
	}
	if suspend.GetActivationId() != activation || !suspend.GetTransitioned() {
		t.Errorf("suspend receipt = %v, want a transition of activation %s", suspend, activation)
	}
}

// A quarantined Actor is refused by every path that would run, restore,
// revert, copy or delete it, and by the egress policy the gateways fetch,
// even after the policy is updated.
func TestActorQuarantine_RefusesEveryPath(t *testing.T) {
	f := newReceiptFixture(t, "ns-quarantine-refuse")
	createEgressPolicy(t, f.tc, f.ref)
	f.resume()
	// A node-local snapshot makes the suspended Actor revertable.
	f.tc.fakeAtelet.Lock.Lock()
	f.tc.fakeAtelet.KeepCheckpointLocal = true
	f.tc.fakeAtelet.Lock.Unlock()
	f.mustQuarantine(&ateapi.QuarantineActorRequest{RequestId: "q-1"})
	f.restores()
	before := f.get()

	for _, req := range []*ateapi.ResumeActorRequest{{}, {RequestId: "resume-1"}} {
		req.Actor = f.ref
		_, err := f.tc.client.ResumeActor(f.ctx, req)
		wantQuarantined(t, "ResumeActor", err)
	}
	_, err := f.tc.client.RevertActor(f.ctx, &ateapi.RevertActorRequest{Actor: f.ref})
	wantQuarantined(t, "RevertActor", err)
	_, err = f.tc.client.DeleteActor(f.ctx, &ateapi.DeleteActorRequest{Actor: f.ref})
	wantQuarantined(t, "DeleteActor", err)
	_, err = f.tc.client.CreateTag(f.ctx, &ateapi.CreateTagRequest{Tag: &ateapi.Tag{
		Metadata:    &ateapi.ResourceMetadata{Atespace: testAtespace, Name: "evidence"},
		Scope:       ateapi.TagScope_TAG_SCOPE_ATESPACE,
		SourceActor: f.ref,
	}})
	wantQuarantined(t, "CreateTag", err)
	_, err = f.tc.client.MintActorJwt(f.ctx, &ateapi.MintActorJwtRequest{Actor: f.ref, Audiences: []string{"api.example.com"}, ExpirationSeconds: 600})
	wantQuarantined(t, "MintActorJwt", err)
	_, err = f.tc.client.GetActorEgressPolicy(f.ctx, &ateapi.GetActorEgressPolicyRequest{Actor: f.ref, Name: "default"})
	wantQuarantined(t, "GetActorEgressPolicy", err)
	f.wantUnchanged(before)
	if f.restores() {
		t.Errorf("atelet was asked to restore a quarantined Actor")
	}

	// An ordinary policy update is stored, and the gateways are still
	// refused.
	stored, err := f.tc.persistence.GetEgressPolicy(f.ctx, resources.ActorRefFromObjectRef(f.ref))
	if err != nil {
		t.Fatalf("stored egress policy: %v", err)
	}
	stored.Rules = []*ateapi.EgressRule{{TlsPassthrough: &ateapi.TlsPassthroughRule{Hostnames: []string{"*"}, Ports: &ateapi.Ports{Numbers: []int32{443}}}}}
	if _, err := f.tc.client.UpdateActorEgressPolicy(f.ctx, &ateapi.UpdateActorEgressPolicyRequest{Actor: f.ref, EgressPolicy: stored}); err != nil {
		t.Fatalf("UpdateActorEgressPolicy: %v", err)
	}
	_, err = f.tc.client.GetActorEgressPolicy(f.ctx, &ateapi.GetActorEgressPolicyRequest{Actor: f.ref, Name: "default"})
	wantQuarantined(t, "GetActorEgressPolicy after an update", err)
}

// Once quarantined, the Actor's worker gets no new certificate, also between
// the write that places the quarantine and the suspend, while the Actor
// still runs. The quarantine here is that first write alone.
func TestActorQuarantine_NoCertificateAfterQuarantine(t *testing.T) {
	f := newReceiptFixture(t, "ns-quarantine-cert")
	activation := f.resume()
	actor := f.get()
	minter := newActivationMinter(t, f.tc, "node1", actor.GetStatus().GetWorkerAssignment().GetWorker().GetName(), f.ref, actor.GetMetadata().GetUid())
	minter.mustMint("before the quarantine", activation)

	actorRef := resources.ActorRefFromObjectRef(f.ref)
	if _, _, err := f.tc.persistence.PlaceActorHold(f.ctx, actorRef, func(*ateapi.Actor) error { return nil }, func(stored *ateapi.Actor) *ateapi.LifecycleReceipt {
		return &ateapi.LifecycleReceipt{
			RequestId:  "q-1",
			ActorUid:   stored.GetMetadata().GetUid(),
			Hold:       &ateapi.ActorHold{Holder: "quarantine", RequestId: "q-1"},
			Quarantine: &ateapi.ActorQuarantine{RequestId: "q-1", ActivationId: activation},
		}
	}); err != nil {
		t.Fatalf("PlaceActorHold: %v", err)
	}
	if got := f.get().GetStatus(); got.GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING || got.GetActivationId() != activation {
		t.Fatalf("status = %v, want RUNNING at %s", got, activation)
	}
	if _, code := minter.mint(activation); code != codes.FailedPrecondition {
		t.Fatalf("mint for a quarantined Actor: code %v, want FailedPrecondition", code)
	}
}

// A failed checkpoint falls back to terminating the same activation. A failed
// terminate leaves the fallback pending, and a retry finishes it.
func TestActorQuarantine_RetryAfterFailedSuspend(t *testing.T) {
	f := newReceiptFixture(t, "ns-quarantine-failed")
	activation := f.resume()
	f.tc.fakeAtelet.Lock.Lock()
	f.tc.fakeAtelet.FailCheckpoint = errors.New("checkpoint failed")
	f.tc.fakeAtelet.FailTerminate = status.Error(codes.Unavailable, "terminate failed")
	f.tc.fakeAtelet.Lock.Unlock()
	if _, err := f.quarantine(detector, &ateapi.QuarantineActorRequest{ExpectedActivation: activation, RequestId: "q-1"}); err == nil {
		t.Fatalf("QuarantineActor succeeded although termination failed")
	}
	failed := f.get()
	if failed.GetStatus().GetQuarantine() == nil {
		t.Fatalf("the failed termination left no quarantine")
	}
	stored, err := f.tc.persistence.GetActorLifecycleReceipt(f.ctx, resources.ActorRefFromObjectRef(f.ref), "q-1")
	if err != nil {
		t.Fatalf("quarantine receipt: %v", err)
	}
	if !cmp.Equal(stored.GetSteps(), stepsPlaced) || !stored.GetTerminatePending() {
		t.Errorf("receipt after the failed terminate = %v, want pending with steps %v", stored, stepsPlaced)
	}

	f.tc.fakeAtelet.Lock.Lock()
	f.tc.fakeAtelet.FailTerminate = nil
	f.tc.fakeAtelet.CheckpointCalled = false
	f.tc.fakeAtelet.Lock.Unlock()
	retried := f.mustQuarantine(&ateapi.QuarantineActorRequest{ExpectedActivation: activation, RequestId: "q-1"})
	if got := retried.GetReceipt(); !cmp.Equal(got.GetSteps(), stepsTerminated) || got.GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED || got.GetTerminatePending() {
		t.Errorf("retried receipt = %v, want SUSPENDED with steps %v", got, stepsTerminated)
	}
	if f.checkpoints() {
		t.Errorf("termination retry checkpointed the actor again")
	}
	if got := f.get().GetStatus(); got.GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED || got.GetActivationId() != "" {
		t.Errorf("status after termination = %v, want SUSPENDED without activation", got)
	}
	// Finished, the receipt is returned as it is.
	again := f.mustQuarantine(&ateapi.QuarantineActorRequest{ExpectedActivation: activation, RequestId: "q-1"})
	if diff := cmp.Diff(retried.GetReceipt(), again.GetReceipt(), protocmp.Transform()); diff != "" {
		t.Errorf("second retry (-first retry +second):\n%s", diff)
	}
}

func TestActorQuarantine_ConcurrentTerminateRetries(t *testing.T) {
	f := newReceiptFixture(t, "ns-quarantine-terminate-retries")
	activation := f.resume()
	f.tc.fakeAtelet.Lock.Lock()
	f.tc.fakeAtelet.FailCheckpoint = errors.New("checkpoint failed")
	f.tc.fakeAtelet.FailTerminate = status.Error(codes.Unavailable, "terminate failed")
	f.tc.fakeAtelet.Lock.Unlock()
	if _, err := f.quarantine(detector, &ateapi.QuarantineActorRequest{ExpectedActivation: activation, RequestId: "q-1"}); err == nil {
		t.Fatal("QuarantineActor succeeded despite failed termination")
	}
	f.tc.fakeAtelet.Lock.Lock()
	f.tc.fakeAtelet.FailTerminate = nil
	f.tc.fakeAtelet.Lock.Unlock()
	const calls = 4
	receipts := make(chan *ateapi.LifecycleReceipt, calls)
	errs := make(chan error, calls)
	for range calls {
		go func() {
			resp, err := f.quarantine(detector, &ateapi.QuarantineActorRequest{ExpectedActivation: activation, RequestId: "q-1"})
			if err != nil {
				errs <- err
				return
			}
			receipts <- resp.GetReceipt()
		}()
	}
	var first *ateapi.LifecycleReceipt
	for range calls {
		select {
		case err := <-errs:
			wantCode(t, "concurrent retry", err, codes.Aborted)
		case r := <-receipts:
			if first == nil {
				first = r
			} else if diff := cmp.Diff(first, r, protocmp.Transform()); diff != "" {
				t.Errorf("concurrent receipts differ (-first +other):\n%s", diff)
			}
		}
	}
	final := f.mustQuarantine(&ateapi.QuarantineActorRequest{ExpectedActivation: activation, RequestId: "q-1"}).GetReceipt()
	if !cmp.Equal(final.GetSteps(), stepsTerminated) {
		t.Errorf("receipt = %v, want terminated step", final)
	}
	if first != nil {
		if diff := cmp.Diff(first, final, protocmp.Transform()); diff != "" {
			t.Errorf("final receipt differs from concurrent receipt (-first +final):\n%s", diff)
		}
	}
}

func TestActorQuarantine_TerminatesAfterFailedCheckpoint(t *testing.T) {
	f := newReceiptFixture(t, "ns-quarantine-terminate")
	f.resume()
	if _, err := f.suspend(&ateapi.SuspendActorRequest{Options: &ateapi.SuspendOptions{Mode: ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS}}); err != nil {
		t.Fatalf("first SuspendActor: %v", err)
	}
	before := f.get().GetStatus().GetSnapshots()
	activation := f.resume()
	f.restores()
	uid := f.get().GetMetadata().GetUid()
	f.tc.fakeAtelet.Lock.Lock()
	f.tc.fakeAtelet.FailCheckpoint = errors.New("checkpoint failed")
	f.tc.fakeAtelet.TerminateCalled = false
	f.tc.fakeAtelet.Lock.Unlock()
	resp := f.mustQuarantine(&ateapi.QuarantineActorRequest{ExpectedUid: uid, ExpectedActivation: activation, RequestId: "q-1"})
	if got := resp.GetReceipt(); !cmp.Equal(got.GetSteps(), stepsTerminated) || got.GetTerminatePending() || got.GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("receipt = %v, want terminated suspension", got)
	}
	f.tc.fakeAtelet.Lock.Lock()
	terminated := f.tc.fakeAtelet.TerminateCalled
	terminatedUID := f.tc.fakeAtelet.TerminateRequest.GetActorUid()
	f.tc.fakeAtelet.Lock.Unlock()
	if !terminated || terminatedUID != uid {
		t.Fatalf("atelet Terminate called = %t, UID = %q; want true, %q", terminated, terminatedUID, uid)
	}
	if got := f.get().GetStatus(); got.GetActivationId() != "" || got.GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED ||
		len(got.GetSnapshots()) != len(before) || got.GetSnapshots()[0].GetUuid() != before[0].GetUuid() {
		t.Fatalf("status = %v, want SUSPENDED with the last durable snapshot", got)
	}
	if _, err := f.releaseQuarantine(securityAdmin, "q-1", "release-1"); err != nil {
		t.Fatalf("ReleaseQuarantine: %v", err)
	}
	if next := f.resume(); next == activation || next == "" {
		t.Fatalf("next activation = %q, want new activation", next)
	}
	if !f.restores() {
		t.Errorf("resume did not restore the durable snapshot")
	}
}

func TestActorQuarantine_PendingTerminateRejectsLaterActivation(t *testing.T) {
	f := newReceiptFixture(t, "ns-quarantine-later-activation")
	activation := f.resume()
	f.tc.fakeAtelet.Lock.Lock()
	f.tc.fakeAtelet.FailCheckpoint = errors.New("checkpoint failed")
	f.tc.fakeAtelet.FailTerminate = status.Error(codes.Unavailable, "terminate failed")
	f.tc.fakeAtelet.Lock.Unlock()
	if _, err := f.quarantine(detector, &ateapi.QuarantineActorRequest{ExpectedActivation: activation, RequestId: "q-1"}); err == nil {
		t.Fatal("QuarantineActor succeeded despite failed termination")
	}
	current := f.get()
	actorRef := resources.ActorRefFromObjectRef(f.ref)
	if _, err := f.tc.persistence.UpdateActor(f.ctx, actorRef, store.PreconditionFrom(current), func(actor *ateapi.Actor) error {
		actor.Status.ActivationId = "later-activation"
		return nil
	}); err != nil {
		t.Fatalf("replace activation: %v", err)
	}
	f.tc.fakeAtelet.Lock.Lock()
	f.tc.fakeAtelet.TerminateCalled = false
	f.tc.fakeAtelet.FailTerminate = nil
	f.tc.fakeAtelet.Lock.Unlock()
	resp := f.mustQuarantine(&ateapi.QuarantineActorRequest{ExpectedActivation: activation, RequestId: "q-1"})
	f.tc.fakeAtelet.Lock.Lock()
	terminated := f.tc.fakeAtelet.TerminateCalled
	f.tc.fakeAtelet.Lock.Unlock()
	if terminated {
		t.Error("pending termination reached the later activation")
	}
	if got := resp.GetReceipt(); got.GetTerminatePending() || slices.Contains(got.GetSteps(), ateapi.QuarantineStep_QUARANTINE_STEP_TERMINATED) {
		t.Errorf("receipt = %v, want no terminated step", got)
	}
}

// A stopped actor with a pending termination receipt cannot prove whether
// this quarantine stopped it. The retry retains the pending receipt.
func TestActorQuarantine_PendingTerminateOutcomeUnknown(t *testing.T) {
	f := newReceiptFixture(t, "ns-quarantine-outcome-unknown")
	activation := f.resume()
	f.tc.fakeAtelet.Lock.Lock()
	f.tc.fakeAtelet.FailCheckpoint = errors.New("checkpoint failed")
	f.tc.fakeAtelet.FailTerminate = status.Error(codes.Unavailable, "terminate failed")
	f.tc.fakeAtelet.Lock.Unlock()
	if _, err := f.quarantine(detector, &ateapi.QuarantineActorRequest{ExpectedActivation: activation, RequestId: "q-1"}); err == nil {
		t.Fatal("QuarantineActor succeeded despite failed termination")
	}
	actorRef := resources.ActorRefFromObjectRef(f.ref)
	current := f.get()
	if _, err := f.tc.persistence.UpdateActor(f.ctx, actorRef, store.PreconditionFrom(current), func(actor *ateapi.Actor) error {
		actor.Status.State = ateapi.ActorState_ACTOR_STATE_SUSPENDED
		actor.Status.ActivationId = ""
		actor.Status.WorkerAssignment = nil
		return nil
	}); err != nil {
		t.Fatalf("record stopped actor: %v", err)
	}
	f.tc.fakeAtelet.Lock.Lock()
	f.tc.fakeAtelet.TerminateCalled = false
	f.tc.fakeAtelet.FailTerminate = nil
	f.tc.fakeAtelet.Lock.Unlock()
	_, err := f.quarantine(detector, &ateapi.QuarantineActorRequest{ExpectedActivation: activation, RequestId: "q-1"})
	wantCode(t, "retry with unknown termination outcome", err, codes.Aborted)
	receipt, err := f.tc.persistence.GetActorLifecycleReceipt(f.ctx, actorRef, "q-1")
	if err != nil {
		t.Fatalf("quarantine receipt: %v", err)
	}
	if !receipt.GetTerminatePending() || !cmp.Equal(receipt.GetSteps(), stepsPlaced) {
		t.Errorf("receipt = %v, want pending without terminated step", receipt)
	}
	f.tc.fakeAtelet.Lock.Lock()
	terminated := f.tc.fakeAtelet.TerminateCalled
	f.tc.fakeAtelet.Lock.Unlock()
	if terminated {
		t.Error("retry terminated an actor with an unknown outcome")
	}
}

// A retry with the same request_id returns the original receipt and does
// nothing else, also after the release. Reuse with other parameters or by
// another caller fails, and so does a second quarantine.
func TestActorQuarantine_RetryReturnsOriginalReceipt(t *testing.T) {
	f := newReceiptFixture(t, "ns-quarantine-retry")
	first := f.mustQuarantine(&ateapi.QuarantineActorRequest{RequestId: "q-1"})
	if first.GetActor() == nil || !cmp.Equal(first.GetReceipt().GetSteps(), stepsPlaced) {
		t.Fatalf("first quarantine of a SUSPENDED Actor = %v, want the Actor and steps %v", first, stepsPlaced)
	}
	before := f.get()

	retried := f.mustQuarantine(&ateapi.QuarantineActorRequest{RequestId: "q-1"})
	if retried.GetActor() != nil {
		t.Errorf("retried quarantine returned an Actor")
	}
	if diff := cmp.Diff(first.GetReceipt(), retried.GetReceipt(), protocmp.Transform()); diff != "" {
		t.Errorf("retried receipt (-first +retry):\n%s", diff)
	}
	f.wantUnchanged(before)

	for name, call := range map[string]func() error{
		"other reason": func() error {
			_, err := f.quarantine(detector, &ateapi.QuarantineActorRequest{RequestId: "q-1", Reason: "other"})
			return err
		},
		"other caller": func() error {
			_, err := f.quarantine(securityA, &ateapi.QuarantineActorRequest{RequestId: "q-1"})
			return err
		},
		"second quarantine": func() error {
			_, err := f.quarantine(detector, &ateapi.QuarantineActorRequest{RequestId: "q-2"})
			return err
		},
	} {
		wantCode(t, name, call(), codes.AlreadyExists)
	}
	f.wantUnchanged(before)

	// After the release, a retry still returns the original receipt and
	// does not quarantine the Actor again.
	if _, err := f.releaseQuarantine(securityAdmin, "q-1", "release-1"); err != nil {
		t.Fatalf("ReleaseQuarantine: %v", err)
	}
	late := f.mustQuarantine(&ateapi.QuarantineActorRequest{RequestId: "q-1"})
	if diff := cmp.Diff(first.GetReceipt(), late.GetReceipt(), protocmp.Transform()); diff != "" {
		t.Errorf("receipt of a retry after the release (-first +retry):\n%s", diff)
	}
	if got := f.get().GetStatus(); got.GetQuarantine() != nil || len(got.GetHolds()) != 0 {
		t.Errorf("a retry after the release quarantined the Actor again: %v", got)
	}
}

// The quarantine hold is not the caller's, so the caller that quarantined
// the Actor cannot remove it through ReleaseActorHold. Authorization of
// ReleaseQuarantine is in model_test.fga.yaml. A release is idempotent and
// does not resume the Actor.
func TestActorQuarantine_Release(t *testing.T) {
	f := newReceiptFixture(t, "ns-quarantine-release")
	f.resume()
	f.mustQuarantine(&ateapi.QuarantineActorRequest{RequestId: "q-1"})
	f.restores()
	before := f.get()

	_, err := f.release(detector, "q-1", "release-hold")
	wantCode(t, "ReleaseActorHold of the quarantine hold", err, codes.PermissionDenied)
	f.wantUnchanged(before)

	resp, err := f.releaseQuarantine(securityAdmin, "q-1", "release-1")
	if err != nil {
		t.Fatalf("ReleaseQuarantine: %v", err)
	}
	r := resp.GetReceipt()
	if !r.GetTransitioned() || r.GetCaller() != "user:"+securityAdmin || r.GetReason() != "investigation closed" ||
		r.GetQuarantine().GetCaller() != "user:"+detector || r.GetHold().GetIncidentId() != "INC-7" {
		t.Errorf("release receipt = %v, want the admin's release of the detector's quarantine", r)
	}
	released := f.get()
	if got := released.GetStatus(); got.GetQuarantine() != nil || len(got.GetHolds()) != 0 || got.GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		t.Errorf("status after release = %v, want SUSPENDED with no quarantine or holds", got)
	}
	if f.restores() {
		t.Errorf("the release restored the Actor")
	}

	retried, err := f.releaseQuarantine(securityAdmin, "q-1", "release-1")
	if err != nil {
		t.Fatalf("retried ReleaseQuarantine: %v", err)
	}
	if diff := cmp.Diff(r, retried.GetReceipt(), protocmp.Transform()); diff != "" {
		t.Errorf("retried release receipt (-first +retry):\n%s", diff)
	}
	again, err := f.releaseQuarantine(securityAdmin, "q-1", "release-2")
	if err != nil {
		t.Fatalf("second ReleaseQuarantine: %v", err)
	}
	if again.GetReceipt().GetTransitioned() {
		t.Errorf("a second release released something: %v", again.GetReceipt())
	}
	f.wantUnchanged(released)
	f.resume()
}

// A quarantine names one Actor by uid. An Actor that replaces it under the
// same name is not quarantined, by a retry of the original request or by a
// request that expects the old uid.
func TestActorQuarantine_ReplacementUnaffected(t *testing.T) {
	f := newReceiptFixture(t, "ns-quarantine-replace")
	oldUID := f.get().GetMetadata().GetUid()
	first := f.mustQuarantine(&ateapi.QuarantineActorRequest{ExpectedUid: oldUID, RequestId: "q-1"})
	pending := proto.CloneOf(first.GetReceipt())
	pending.TerminatePending = true
	if err := f.tc.persistence.ReplaceActorHoldReceipt(f.ctx, resources.ActorRefFromObjectRef(f.ref), pending); err != nil {
		t.Fatalf("ReplaceActorHoldReceipt: %v", err)
	}
	if _, err := f.releaseQuarantine(securityAdmin, "q-1", "release-1"); err != nil {
		t.Fatalf("ReleaseQuarantine: %v", err)
	}
	if _, err := f.tc.client.DeleteActor(f.ctx, &ateapi.DeleteActorRequest{Actor: f.ref}); err != nil {
		t.Fatalf("DeleteActor: %v", err)
	}
	replacement := f.create()
	before := f.get()
	f.tc.fakeAtelet.Lock.Lock()
	f.tc.fakeAtelet.TerminateCalled = false
	f.tc.fakeAtelet.Lock.Unlock()

	retried := f.mustQuarantine(&ateapi.QuarantineActorRequest{ExpectedUid: oldUID, RequestId: "q-1"})
	if diff := cmp.Diff(pending, retried.GetReceipt(), protocmp.Transform()); diff != "" {
		t.Errorf("retried receipt (-first +retry):\n%s", diff)
	}
	if f.tc.fakeAtelet.TerminateCalled {
		t.Errorf("a retry terminated the replacement Actor")
	}
	_, err := f.quarantine(detector, &ateapi.QuarantineActorRequest{ExpectedUid: oldUID, RequestId: "q-2"})
	wantCode(t, "QuarantineActor expecting the replaced uid", err, codes.FailedPrecondition)
	f.wantUnchanged(before)
	if got := f.get().GetStatus(); got.GetQuarantine() != nil || len(got.GetHolds()) != 0 {
		t.Errorf("replacement %s status = %v, want no quarantine or holds", replacement.GetMetadata().GetUid(), got)
	}
	f.resume()
}

// A retry of a quarantine whose suspend never finished does not suspend
// the Actor once the quarantine is released. The pending quarantine here is
// the first write alone, as a call that failed before its suspend leaves it.
func TestActorQuarantine_RetryAfterReleaseDoesNotSuspend(t *testing.T) {
	f := newReceiptFixture(t, "ns-quarantine-late")
	activation := f.resume()
	actorRef := resources.ActorRefFromObjectRef(f.ref)
	caller := "user:" + detector
	if _, _, err := f.tc.persistence.PlaceActorHold(f.ctx, actorRef, func(*ateapi.Actor) error { return nil }, func(stored *ateapi.Actor) *ateapi.LifecycleReceipt {
		return &ateapi.LifecycleReceipt{
			RequestId:        "q-1",
			Operation:        ateapi.LifecycleOperation_LIFECYCLE_OPERATION_QUARANTINE,
			Actor:            f.ref,
			ActorUid:         stored.GetMetadata().GetUid(),
			ActivationId:     activation,
			Transitioned:     true,
			State:            ateapi.ActorState_ACTOR_STATE_RUNNING,
			TerminatePending: true,
			Caller:           caller,
			Reason:           "credential exfiltration",
			Steps:            stepsPlaced,
			SuspendRequestId: "q-1/suspend",
			Hold:             &ateapi.ActorHold{Holder: "quarantine", RequestId: "q-1", Reason: "credential exfiltration", IncidentId: "INC-7"},
			Quarantine:       &ateapi.ActorQuarantine{RequestId: "q-1", Caller: caller, Reason: "credential exfiltration", IncidentId: "INC-7", ActivationId: activation},
		}
	}); err != nil {
		t.Fatalf("PlaceActorHold: %v", err)
	}
	if _, err := f.releaseQuarantine(securityAdmin, "q-1", "release-1"); err != nil {
		t.Fatalf("ReleaseQuarantine: %v", err)
	}
	f.checkpoints()
	before := f.get()

	retried := f.mustQuarantine(&ateapi.QuarantineActorRequest{RequestId: "q-1"})
	if got := retried.GetReceipt(); !cmp.Equal(got.GetSteps(), stepsPlaced) {
		t.Errorf("retried receipt = %v, want the original with steps %v", got, stepsPlaced)
	}
	if f.checkpoints() {
		t.Errorf("a retry after the release suspended the Actor")
	}
	if f.tc.fakeAtelet.TerminateCalled {
		t.Errorf("a retry after the release terminated the Actor")
	}
	f.wantUnchanged(before)
}

// Concurrent calls with one request_id return the same receipt.
func TestActorQuarantine_ConcurrentRetries(t *testing.T) {
	f := newReceiptFixture(t, "ns-quarantine-concurrent")
	const calls = 4
	receipts := make(chan *ateapi.LifecycleReceipt, calls)
	errs := make(chan error, calls)
	for range calls {
		go func() {
			resp, err := f.quarantine(detector, &ateapi.QuarantineActorRequest{RequestId: "q-1"})
			if err != nil {
				errs <- err
				return
			}
			receipts <- resp.GetReceipt()
		}()
	}
	var first *ateapi.LifecycleReceipt
	for range calls {
		select {
		case err := <-errs:
			// Only a lost store race may be refused, and it asks for a retry.
			wantCode(t, "concurrent QuarantineActor", err, codes.Aborted)
		case r := <-receipts:
			if first == nil {
				first = r
			} else if diff := cmp.Diff(first, r, protocmp.Transform()); diff != "" {
				t.Errorf("concurrent receipts differ (-first +other):\n%s", diff)
			}
		}
	}
	if got := holdersOf(f.get()); !cmp.Equal(got, []string{"quarantine/q-1"}) {
		t.Errorf("holds = %v, want one quarantine", got)
	}
}

// A revert under way discards snapshots, so a REVERTING Actor can't be
// quarantined, and a quarantined Actor left REVERTING can't be reverted.
func TestActorQuarantine_Reverting(t *testing.T) {
	f := newReceiptFixture(t, "ns-quarantine-reverting")
	actorRef := resources.ActorRefFromObjectRef(f.ref)
	current := f.get()
	if _, err := f.tc.persistence.UpdateActor(f.ctx, actorRef, store.PreconditionFrom(current), func(a *ateapi.Actor) error {
		a.Status.State = ateapi.ActorState_ACTOR_STATE_REVERTING
		return nil
	}); err != nil {
		t.Fatalf("UpdateActor: %v", err)
	}
	before := f.get()
	_, err := f.quarantine(detector, &ateapi.QuarantineActorRequest{RequestId: "q-1"})
	wantCode(t, "QuarantineActor of a REVERTING Actor", err, codes.FailedPrecondition)
	f.wantUnchanged(before)

	if _, _, err := f.tc.persistence.PlaceActorHold(f.ctx, actorRef, func(*ateapi.Actor) error { return nil }, func(stored *ateapi.Actor) *ateapi.LifecycleReceipt {
		return &ateapi.LifecycleReceipt{RequestId: "q-2", ActorUid: stored.GetMetadata().GetUid(),
			Hold: &ateapi.ActorHold{Holder: "quarantine", RequestId: "q-2"}, Quarantine: &ateapi.ActorQuarantine{RequestId: "q-2"}}
	}); err != nil {
		t.Fatalf("PlaceActorHold: %v", err)
	}
	_, err = f.tc.client.RevertActor(f.ctx, &ateapi.RevertActorRequest{Actor: f.ref})
	wantQuarantined(t, "RevertActor of a quarantined REVERTING Actor", err)
}
