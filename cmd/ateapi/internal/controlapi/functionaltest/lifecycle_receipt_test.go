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
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/google/go-cmp/cmp"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/internal/resources"
)

// receiptFixture is a running Actor and the client calls the receipt tests
// make on it.
type receiptFixture struct {
	t   *testing.T
	tc  *testContext
	ctx context.Context
	ref *ateapi.ObjectRef
}

func newReceiptFixture(t *testing.T, base string) *receiptFixture {
	t.Helper()
	ns := namespaceForTest(base)
	tc := setupTest(t, ns)
	t.Cleanup(tc.cleanup)
	createTemplate(t, tc, ns)
	createWorkerPod(t, tc, ns, "worker-1", "node1", "pool1")
	f := &receiptFixture{t: t, tc: tc, ctx: context.Background(), ref: &ateapi.ObjectRef{Atespace: testAtespace, Name: "id1"}}
	f.create()
	return f
}

func (f *receiptFixture) create() *ateapi.Actor {
	f.t.Helper()
	created, err := f.tc.client.CreateActor(f.ctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: f.ref.GetAtespace(), Name: f.ref.GetName()},
		ActorTemplate: &ateapi.ObjectRef{Atespace: testAtespace, Name: "tmpl1"},
	}})
	if err != nil {
		f.t.Fatalf("CreateActor: %v", err)
	}
	return created
}

func (f *receiptFixture) get() *ateapi.Actor {
	f.t.Helper()
	actor, err := f.tc.client.GetActor(f.ctx, &ateapi.GetActorRequest{Actor: f.ref})
	if err != nil {
		f.t.Fatalf("GetActor: %v", err)
	}
	return actor
}

// resume resumes the Actor without conditions and returns its activation.
func (f *receiptFixture) resume() string {
	f.t.Helper()
	resp, err := f.tc.client.ResumeActor(f.ctx, &ateapi.ResumeActorRequest{Actor: f.ref})
	if err != nil {
		f.t.Fatalf("ResumeActor: %v", err)
	}
	return resp.GetActor().GetStatus().GetActivationId()
}

func (f *receiptFixture) suspend(req *ateapi.SuspendActorRequest) (*ateapi.SuspendActorResponse, error) {
	req.Actor = f.ref
	return f.tc.client.SuspendActor(f.ctx, req)
}

// checkpoints reports whether atelet was asked to checkpoint since the last
// call, and clears it.
func (f *receiptFixture) checkpoints() bool {
	a := f.tc.fakeAtelet
	a.Lock.Lock()
	defer a.Lock.Unlock()
	called := a.CheckpointCalled
	a.CheckpointCalled, a.CheckpointRequest = false, nil
	return called
}

// wantUnchanged fails unless the Actor is at the same version as before.
func (f *receiptFixture) wantUnchanged(before *ateapi.Actor) {
	f.t.Helper()
	if diff := cmp.Diff(before, f.get(), protocmp.Transform()); diff != "" {
		f.t.Errorf("Actor changed (-before +after):\n%s", diff)
	}
}

// A call conditioned on an activation the Actor no longer holds is refused,
// changes nothing and stores no receipt, whether it suspends, resumes or
// deletes.
func TestLifecycleReceipt_StaleRun(t *testing.T) {
	f := newReceiptFixture(t, "ns-receipt-stale")
	stale := f.resume()
	if _, err := f.suspend(&ateapi.SuspendActorRequest{}); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	current := f.resume()
	f.checkpoints()
	before := f.get()

	_, err := f.suspend(&ateapi.SuspendActorRequest{ExpectedActivation: stale, RequestId: "stale-suspend"})
	wantCode(t, "SuspendActor for the earlier activation", err, codes.FailedPrecondition)
	_, err = f.tc.client.ResumeActor(f.ctx, &ateapi.ResumeActorRequest{Actor: f.ref, ExpectedActivation: stale, RequestId: "stale-resume"})
	wantCode(t, "ResumeActor for the earlier activation", err, codes.FailedPrecondition)
	_, err = f.tc.client.ResumeActor(f.ctx, &ateapi.ResumeActorRequest{Actor: f.ref, ExpectedActivation: stale})
	wantCode(t, "ResumeActor for the earlier activation, without a request_id", err, codes.FailedPrecondition)
	_, err = f.tc.client.DeleteActor(f.ctx, &ateapi.DeleteActorRequest{Actor: f.ref, Options: &ateapi.DeleteOptions{ExpectedActivation: stale}})
	wantCode(t, "DeleteActor for the earlier activation", err, codes.FailedPrecondition)
	if f.checkpoints() {
		t.Error("a refused call checkpointed the Actor")
	}
	f.wantUnchanged(before)

	// The refused calls stored nothing, so the same request_id with the
	// current activation is a first call.
	resp, err := f.suspend(&ateapi.SuspendActorRequest{ExpectedActivation: current, RequestId: "stale-suspend"})
	if err != nil {
		t.Fatalf("SuspendActor for the current activation: %v", err)
	}
	if got := resp.GetReceipt().GetActivationId(); got != current {
		t.Errorf("receipt activation_id = %q, want %q", got, current)
	}

	// A SUSPENDED Actor holds no activation, so a delete for one is refused.
	_, err = f.tc.client.DeleteActor(f.ctx, &ateapi.DeleteActorRequest{Actor: f.ref, Options: &ateapi.DeleteOptions{ExpectedActivation: current}})
	wantCode(t, "DeleteActor of a SUSPENDED Actor for its last activation", err, codes.FailedPrecondition)
}

// A call conditioned on the uid of a deleted Actor does not touch the Actor
// that replaced it under the same name, and a retry of a receipt the deleted
// Actor stored returns that receipt.
func TestLifecycleReceipt_Replacement(t *testing.T) {
	f := newReceiptFixture(t, "ns-receipt-replace")
	original := f.get().GetMetadata().GetUid()
	first := f.resume()
	suspend := &ateapi.SuspendActorRequest{
		ExpectedUid:        original,
		ExpectedActivation: first,
		RequestId:          "incident-1/suspend",
		Options:            &ateapi.SuspendOptions{Mode: ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS},
	}
	resp, err := f.suspend(proto.CloneOf(suspend))
	if err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	receipt := resp.GetReceipt()

	if _, err := f.tc.client.DeleteActor(f.ctx, &ateapi.DeleteActorRequest{Actor: f.ref}); err != nil {
		t.Fatalf("DeleteActor: %v", err)
	}
	replacement := f.create().GetMetadata().GetUid()
	f.resume()
	f.checkpoints()
	before := f.get()

	_, err = f.suspend(&ateapi.SuspendActorRequest{ExpectedUid: original})
	wantCode(t, "SuspendActor for the deleted uid", err, codes.FailedPrecondition)
	_, err = f.tc.client.ResumeActor(f.ctx, &ateapi.ResumeActorRequest{Actor: f.ref, ExpectedUid: original, RequestId: "incident-1/resume"})
	wantCode(t, "ResumeActor for the deleted uid", err, codes.FailedPrecondition)
	_, err = f.tc.client.DeleteActor(f.ctx, &ateapi.DeleteActorRequest{Actor: f.ref, Options: &ateapi.DeleteOptions{Uid: original}})
	if code := status.Code(err); code == codes.OK {
		t.Fatalf("DeleteActor for the deleted uid succeeded")
	}

	retried, err := f.suspend(proto.CloneOf(suspend))
	if err != nil {
		t.Fatalf("retried SuspendActor: %v", err)
	}
	if diff := cmp.Diff(receipt, retried.GetReceipt(), protocmp.Transform()); diff != "" {
		t.Errorf("retried receipt (-original +got):\n%s", diff)
	}
	if got := retried.GetReceipt().GetActorUid(); got != original || got == replacement {
		t.Errorf("retried receipt actor_uid = %q, want the deleted Actor's %q", got, original)
	}
	if retried.GetActor() != nil {
		t.Errorf("replayed response carries an Actor, want none")
	}
	if f.checkpoints() {
		t.Error("the retry checkpointed the replacement")
	}
	f.wantUnchanged(before)
}

// A retry with the same request_id returns the original receipt and does
// nothing, even after the Actor moved to a later activation, so a retried
// suspend never stops a later run and a retried resume never restarts a
// suspended Actor.
func TestLifecycleReceipt_Retry(t *testing.T) {
	f := newReceiptFixture(t, "ns-receipt-retry")
	uid := f.get().GetMetadata().GetUid()

	resume := &ateapi.ResumeActorRequest{Actor: f.ref, ExpectedUid: uid, RequestId: "r-resume"}
	resumed, err := f.tc.client.ResumeActor(f.ctx, proto.CloneOf(resume))
	if err != nil {
		t.Fatalf("ResumeActor: %v", err)
	}
	first := resumed.GetActor().GetStatus().GetActivationId()
	wantResume := &ateapi.LifecycleReceipt{
		RequestId:    "r-resume",
		Operation:    ateapi.LifecycleOperation_LIFECYCLE_OPERATION_RESUME,
		Actor:        f.ref,
		ActorUid:     uid,
		ExpectedUid:  uid,
		ActivationId: first,
		Transitioned: true,
		State:        ateapi.ActorState_ACTOR_STATE_RUNNING,
	}
	ignoreTimes := protocmp.IgnoreFields(&ateapi.LifecycleReceipt{}, "create_time", "expire_time")
	if diff := cmp.Diff(wantResume, resumed.GetReceipt(), protocmp.Transform(), ignoreTimes); diff != "" {
		t.Errorf("resume receipt (-want +got):\n%s", diff)
	}
	if !resumed.GetResumed() {
		t.Error("first ResumeActor resumed = false, want true")
	}

	suspend := &ateapi.SuspendActorRequest{
		ExpectedUid:        uid,
		ExpectedActivation: first,
		RequestId:          "r-suspend",
		Options:            &ateapi.SuspendOptions{Mode: ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS},
	}
	suspended, err := f.suspend(proto.CloneOf(suspend))
	if err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	wantSuspend := &ateapi.LifecycleReceipt{
		RequestId:          "r-suspend",
		Operation:          ateapi.LifecycleOperation_LIFECYCLE_OPERATION_SUSPEND,
		Actor:              f.ref,
		ActorUid:           uid,
		ExpectedUid:        uid,
		ExpectedActivation: first,
		ActivationId:       first,
		Transitioned:       true,
		State:              ateapi.ActorState_ACTOR_STATE_SUSPENDED,
		Mode:               ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS,
		SnapshotDurable:    true,
	}
	if diff := cmp.Diff(wantSuspend, suspended.GetReceipt(), protocmp.Transform(), ignoreTimes); diff != "" {
		t.Errorf("suspend receipt (-want +got):\n%s", diff)
	}
	if got := suspended.GetActor().GetStatus().GetState(); got != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		t.Errorf("suspended Actor state = %v, want SUSPENDED", got)
	}

	// Retrying the resume after the suspend replays the resume's receipt and
	// leaves the Actor SUSPENDED.
	before := f.get()
	replayedResume, err := f.tc.client.ResumeActor(f.ctx, proto.CloneOf(resume))
	if err != nil {
		t.Fatalf("retried ResumeActor: %v", err)
	}
	if diff := cmp.Diff(resumed.GetReceipt(), replayedResume.GetReceipt(), protocmp.Transform()); diff != "" {
		t.Errorf("retried resume receipt (-original +got):\n%s", diff)
	}
	if replayedResume.GetResumed() || replayedResume.GetActor() != nil {
		t.Errorf("replayed resume response = resumed %v, actor %v; want false and none", replayedResume.GetResumed(), replayedResume.GetActor())
	}
	f.wantUnchanged(before)

	// Retrying the suspend after a later resume replays its receipt and
	// leaves the later activation running.
	second := f.resume()
	f.checkpoints()
	before = f.get()
	replayedSuspend, err := f.suspend(proto.CloneOf(suspend))
	if err != nil {
		t.Fatalf("retried SuspendActor: %v", err)
	}
	if diff := cmp.Diff(suspended.GetReceipt(), replayedSuspend.GetReceipt(), protocmp.Transform()); diff != "" {
		t.Errorf("retried suspend receipt (-original +got):\n%s", diff)
	}
	if f.checkpoints() {
		t.Error("the retried suspend checkpointed the later activation")
	}
	f.wantUnchanged(before)
	if got := before.GetStatus().GetActivationId(); got != second {
		t.Errorf("Actor activation = %q, want the later %q", got, second)
	}

	// A call that finds the Actor already in the target state stores a
	// receipt too, so its retry stays a no-op after the Actor moves on.
	noop, err := f.tc.client.ResumeActor(f.ctx, &ateapi.ResumeActorRequest{Actor: f.ref, RequestId: "r-noop"})
	if err != nil {
		t.Fatalf("ResumeActor of a RUNNING Actor: %v", err)
	}
	if r := noop.GetReceipt(); r.GetTransitioned() || r.GetActivationId() != second || r.GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Errorf("no-op resume receipt = %v, want not transitioned, RUNNING under %q", r, second)
	}
	if _, err := f.suspend(&ateapi.SuspendActorRequest{}); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	before = f.get()
	if _, err := f.tc.client.ResumeActor(f.ctx, &ateapi.ResumeActorRequest{Actor: f.ref, RequestId: "r-noop"}); err != nil {
		t.Fatalf("retried no-op ResumeActor: %v", err)
	}
	f.wantUnchanged(before)
}

// A suspend whose checkpoint fails after it marked the Actor SUSPENDING
// leaves an unfinished receipt. Its retry finishes that suspend and stores
// the final receipt for the same activation.
func TestLifecycleReceipt_RetryFinishesInterruptedSuspend(t *testing.T) {
	f := newReceiptFixture(t, "ns-receipt-interrupted")
	activation := f.resume()
	suspend := &ateapi.SuspendActorRequest{ExpectedActivation: activation, RequestId: "r-interrupted"}

	f.tc.fakeAtelet.Lock.Lock()
	f.tc.fakeAtelet.FailCheckpoint = status.Error(codes.Unavailable, "worker unreachable")
	f.tc.fakeAtelet.Lock.Unlock()
	_, err := f.suspend(proto.CloneOf(suspend))
	wantCode(t, "SuspendActor with a failing checkpoint", err, codes.Unavailable)
	if got := f.get().GetStatus().GetState(); got != ateapi.ActorState_ACTOR_STATE_SUSPENDING {
		t.Fatalf("state after the failed checkpoint = %v, want SUSPENDING", got)
	}
	actorRef := resources.ActorRefFromObjectRef(f.ref)
	unfinished, err := f.tc.persistence.GetActorLifecycleReceipt(f.ctx, actorRef, suspend.GetRequestId())
	if err != nil {
		t.Fatalf("GetActorLifecycleReceipt: %v", err)
	}
	if unfinished.GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDING || unfinished.GetActivationId() != activation {
		t.Errorf("unfinished receipt = %v, want SUSPENDING for %q", unfinished, activation)
	}

	f.tc.fakeAtelet.Lock.Lock()
	f.tc.fakeAtelet.FailCheckpoint = nil
	f.tc.fakeAtelet.Lock.Unlock()
	resp, err := f.suspend(proto.CloneOf(suspend))
	if err != nil {
		t.Fatalf("retried SuspendActor: %v", err)
	}
	r := resp.GetReceipt()
	if r.GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED || r.GetActivationId() != activation || !r.GetTransitioned() {
		t.Errorf("final receipt = %v, want SUSPENDED for %q", r, activation)
	}
	if !proto.Equal(r.GetCreateTime(), unfinished.GetCreateTime()) {
		t.Errorf("final receipt create_time = %v, want the unfinished receipt's %v", r.GetCreateTime(), unfinished.GetCreateTime())
	}
}

// Reusing a request_id with other parameters fails and changes nothing.
func TestLifecycleReceipt_ConflictingReuse(t *testing.T) {
	f := newReceiptFixture(t, "ns-receipt-conflict")
	activation := f.resume()
	const requestID = "r-conflict"
	if _, err := f.suspend(&ateapi.SuspendActorRequest{ExpectedActivation: activation, RequestId: requestID}); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	f.resume()
	f.checkpoints()
	before := f.get()

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"other durability mode", func() error {
			_, err := f.suspend(&ateapi.SuspendActorRequest{ExpectedActivation: activation, RequestId: requestID, Options: &ateapi.SuspendOptions{Mode: ateapi.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS}})
			return err
		}},
		{"other expected_activation", func() error {
			_, err := f.suspend(&ateapi.SuspendActorRequest{ExpectedActivation: before.GetStatus().GetActivationId(), RequestId: requestID})
			return err
		}},
		{"no expected_activation", func() error {
			_, err := f.suspend(&ateapi.SuspendActorRequest{RequestId: requestID})
			return err
		}},
		{"other expected_uid", func() error {
			_, err := f.suspend(&ateapi.SuspendActorRequest{ExpectedUid: before.GetMetadata().GetUid(), ExpectedActivation: activation, RequestId: requestID})
			return err
		}},
		{"resume", func() error {
			_, err := f.tc.client.ResumeActor(f.ctx, &ateapi.ResumeActorRequest{Actor: f.ref, ExpectedActivation: activation, RequestId: requestID})
			return err
		}},
	} {
		wantCode(t, tc.name, tc.call(), codes.AlreadyExists)
	}
	if f.checkpoints() {
		t.Error("a conflicting reuse checkpointed the Actor")
	}
	f.wantUnchanged(before)
}

// An asynchronous suspend's receipt reports the snapshot as not yet durable.
// Its retry after the upload finishes still returns that original receipt;
// the Actor shows the upload.
func TestLifecycleReceipt_AsyncSuspend(t *testing.T) {
	f := newReceiptFixture(t, "ns-receipt-async")
	activation := f.resume()
	suspend := &ateapi.SuspendActorRequest{
		ExpectedActivation: activation,
		RequestId:          "r-async",
		Options:            &ateapi.SuspendOptions{Mode: ateapi.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS},
	}
	resp, err := f.suspend(proto.CloneOf(suspend))
	if err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	r := resp.GetReceipt()
	if r.GetMode() != ateapi.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS || r.GetSnapshotDurable() || r.GetActivationId() != activation || r.GetState() != ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("async receipt = %v, want ASYNCHRONOUS, not durable, SUSPENDED from %q", r, activation)
	}

	// Stand in for atelet's report that the upload finished.
	actorRef := resources.ActorRefFromObjectRef(f.ref)
	suspended := f.get()
	if _, err := f.tc.persistence.UpdateActor(f.ctx, actorRef, store.PreconditionFrom(suspended), func(a *ateapi.Actor) error {
		snaps := a.GetStatus().GetSnapshots()
		snaps[len(snaps)-1].GetDurableSnapshot().State = ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED
		return nil
	}); err != nil {
		t.Fatalf("recording the upload: %v", err)
	}

	retried, err := f.suspend(proto.CloneOf(suspend))
	if err != nil {
		t.Fatalf("retried SuspendActor: %v", err)
	}
	if diff := cmp.Diff(r, retried.GetReceipt(), protocmp.Transform()); diff != "" {
		t.Errorf("retried receipt (-original +got):\n%s", diff)
	}
	snaps := f.get().GetStatus().GetSnapshots()
	if got := snaps[len(snaps)-1].GetDurableSnapshot().GetState(); got != ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED {
		t.Errorf("Actor's latest durable snapshot = %v, want COMPLETED", got)
	}
}

// Only DeleteActor accepts expected_activation.
func TestLifecycleReceipt_ExpectedActivationOnlyForActors(t *testing.T) {
	f := newReceiptFixture(t, "ns-receipt-delete-options")
	_, err := f.tc.client.DeleteActorTemplate(f.ctx, &ateapi.DeleteActorTemplateRequest{
		ActorTemplate: &ateapi.ObjectRef{Atespace: testAtespace, Name: "tmpl1"},
		Options:       &ateapi.DeleteOptions{ExpectedActivation: "01JDQ5W2R3V6Z8B4N7K9M1P0XA"},
	})
	wantCode(t, "DeleteActorTemplate with expected_activation", err, codes.InvalidArgument)
}
