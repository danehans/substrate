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

package functionaltest

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/internal/resources"
)

const (
	securityA = "spiffe://cluster.local/ns/sec/sa/controller-a"
	securityB = "spiffe://cluster.local/ns/sec/sa/controller-b"
)

func (f *receiptFixture) hold(principal string, req *ateapi.HoldActorRequest) (*ateapi.HoldActorResponse, error) {
	req.Actor = f.ref
	if req.Reason == "" {
		req.Reason = "egress to a blocked host"
	}
	return f.tc.client.HoldActor(asPrincipal(f.ctx, principal), req)
}

func (f *receiptFixture) mustHold(principal, requestID string) *ateapi.HoldActorResponse {
	f.t.Helper()
	resp, err := f.hold(principal, &ateapi.HoldActorRequest{RequestId: requestID})
	if err != nil {
		f.t.Fatalf("HoldActor(%s, %s): %v", principal, requestID, err)
	}
	return resp
}

func (f *receiptFixture) release(principal, holdRequestID, requestID string) (*ateapi.ReleaseActorHoldResponse, error) {
	return f.tc.client.ReleaseActorHold(asPrincipal(f.ctx, principal), &ateapi.ReleaseActorHoldRequest{
		Actor:         f.ref,
		HoldRequestId: holdRequestID,
		RequestId:     requestID,
	})
}

func (f *receiptFixture) mustRelease(principal, holdRequestID, requestID string) *ateapi.ReleaseActorHoldResponse {
	f.t.Helper()
	resp, err := f.release(principal, holdRequestID, requestID)
	if err != nil {
		f.t.Fatalf("ReleaseActorHold(%s, %s): %v", principal, holdRequestID, err)
	}
	return resp
}

// wantHeld fails unless err is the refusal of a held Actor, with the
// ErrorInfo the ingress router fails fast on. The domain and reason are
// spelled out because they are what agentgateway's Substrate ingress
// matches, not Substrate's own constants.
func wantHeld(t *testing.T, err error) {
	t.Helper()
	s, _ := status.FromError(err)
	if s.Code() != codes.FailedPrecondition {
		t.Fatalf("error = %v, want FailedPrecondition", err)
	}
	want := &errdetails.ErrorInfo{Domain: "ate.dev", Reason: "TERMINAL_STATE", Metadata: map[string]string{"cause": "ACTOR_HELD"}}
	details := s.Details()
	if len(details) != 1 {
		t.Fatalf("details = %v, want one ErrorInfo", details)
	}
	if got, ok := details[0].(*errdetails.ErrorInfo); !ok || !proto.Equal(got, want) {
		t.Fatalf("details[0] = %v, want %v", details[0], want)
	}
}

func holdersOf(a *ateapi.Actor) []string {
	var out []string
	for _, h := range a.GetStatus().GetHolds() {
		out = append(out, h.GetHolder()+"/"+h.GetRequestId())
	}
	return out
}

// A held Actor is not resumed, whether it is SUSPENDED or RUNNING, with or
// without a request_id, and the refusal stores no receipt.
func TestActorHold_RefusesResume(t *testing.T) {
	f := newReceiptFixture(t, "ns-hold-resume")
	held := f.mustHold(securityA, "hold-1")
	if got, want := holdersOf(held.GetActor()), []string{"user:" + securityA + "/hold-1"}; !cmp.Equal(got, want) {
		t.Fatalf("holds = %v, want %v", got, want)
	}
	before := f.get()
	for _, req := range []*ateapi.ResumeActorRequest{{}, {RequestId: "resume-1"}} {
		req.Actor = f.ref
		_, err := f.tc.client.ResumeActor(f.ctx, req)
		wantHeld(t, err)
	}
	f.wantUnchanged(before)
	if f.tc.fakeAtelet.lastRestoreRequest() != nil {
		t.Errorf("atelet was asked to restore a held Actor")
	}
	if _, err := f.tc.persistence.GetActorLifecycleReceipt(f.ctx, resources.ActorRefFromObjectRef(f.ref), "resume-1"); err == nil {
		t.Errorf("a refused resume stored a receipt")
	}

	// A hold does not stop a RUNNING Actor, but it refuses the resume that
	// every routed request makes.
	f.mustRelease(securityA, "hold-1", "release-1")
	f.resume()
	f.mustHold(securityA, "hold-2")
	before = f.get()
	for _, req := range []*ateapi.ResumeActorRequest{{}, {RequestId: "resume-2"}} {
		req.Actor = f.ref
		_, err := f.tc.client.ResumeActor(f.ctx, req)
		wantHeld(t, err)
	}
	f.wantUnchanged(before)
	if got := before.GetStatus().GetState(); got != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Errorf("state = %v, want RUNNING", got)
	}
	if _, err := f.tc.persistence.GetActorLifecycleReceipt(f.ctx, resources.ActorRefFromObjectRef(f.ref), "resume-2"); err == nil {
		t.Errorf("a refused resume of a RUNNING Actor stored a receipt")
	}
}

// Only the holder releases its hold. A principal that names another
// principal's hold is refused, and its call changes nothing.
func TestActorHold_OnlyTheHolderReleases(t *testing.T) {
	f := newReceiptFixture(t, "ns-hold-owner")
	f.mustHold(securityA, "hold-a")
	before := f.get()

	_, err := f.release(securityB, "hold-a", "release-b")
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("release of another principal's hold = %v, want PermissionDenied", err)
	}
	f.wantUnchanged(before)

	// No principal is checked here except the caller's own: an unauthenticated
	// call is refused.
	if _, err := f.tc.client.ReleaseActorHold(f.ctx, &ateapi.ReleaseActorHoldRequest{Actor: f.ref, HoldRequestId: "hold-a", RequestId: "release-anon"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("release without a principal = %v, want Unauthenticated", err)
	}
	f.wantUnchanged(before)

	if got := f.mustRelease(securityA, "hold-a", "release-a").GetReceipt(); !got.GetTransitioned() {
		t.Errorf("holder's release receipt = %v, want transitioned", got)
	}
	f.resume()
}

// Two holders hold and release independently. The Actor resumes only once
// both have released.
func TestActorHold_TwoHolders(t *testing.T) {
	f := newReceiptFixture(t, "ns-hold-two")
	f.mustHold(securityA, "hold-a")
	f.mustHold(securityB, "hold-b")

	released := f.mustRelease(securityA, "hold-a", "release-a")
	if got, want := holdersOf(released.GetActor()), []string{"user:" + securityB + "/hold-b"}; !cmp.Equal(got, want) {
		t.Fatalf("holds after A's release = %v, want %v", got, want)
	}
	_, err := f.tc.client.ResumeActor(f.ctx, &ateapi.ResumeActorRequest{Actor: f.ref})
	wantHeld(t, err)

	f.mustRelease(securityB, "hold-b", "release-b")
	if got := f.get().GetStatus().GetHolds(); len(got) != 0 {
		t.Fatalf("holds after both releases = %v, want none", got)
	}
	f.resume()
}

// A retry with the same request_id returns the original receipt and does
// nothing else. Reuse with other parameters, or by another principal, fails.
func TestActorHold_RetryReturnsOriginalReceipt(t *testing.T) {
	f := newReceiptFixture(t, "ns-hold-retry")
	first := f.mustHold(securityA, "hold-1")
	if first.GetActor() == nil || !first.GetReceipt().GetTransitioned() {
		t.Fatalf("first hold = %v, want the Actor and a transitioned receipt", first)
	}
	before := f.get()

	retried := f.mustHold(securityA, "hold-1")
	if retried.GetActor() != nil {
		t.Errorf("retried hold returned an Actor")
	}
	if diff := cmp.Diff(first.GetReceipt(), retried.GetReceipt(), protocmp.Transform()); diff != "" {
		t.Errorf("retried hold receipt (-original +got):\n%s", diff)
	}
	f.wantUnchanged(before)

	for name, call := range map[string]func() error{
		"other reason": func() error {
			_, err := f.hold(securityA, &ateapi.HoldActorRequest{RequestId: "hold-1", Reason: "another reason"})
			return err
		},
		"other principal": func() error {
			_, err := f.hold(securityB, &ateapi.HoldActorRequest{RequestId: "hold-1"})
			return err
		},
		"second hold by one holder": func() error {
			_, err := f.hold(securityA, &ateapi.HoldActorRequest{RequestId: "hold-2"})
			return err
		},
		"release under a hold's request_id": func() error {
			_, err := f.release(securityA, "hold-1", "hold-1")
			return err
		},
	} {
		if err := call(); status.Code(err) != codes.AlreadyExists {
			t.Errorf("%s = %v, want AlreadyExists", name, err)
		}
	}
	f.wantUnchanged(before)

	released := f.mustRelease(securityA, "hold-1", "release-1")
	again := f.mustRelease(securityA, "hold-1", "release-1")
	if again.GetActor() != nil {
		t.Errorf("retried release returned an Actor")
	}
	if diff := cmp.Diff(released.GetReceipt(), again.GetReceipt(), protocmp.Transform()); diff != "" {
		t.Errorf("retried release receipt (-original +got):\n%s", diff)
	}

	// A later hold by the same holder is not released by a retry of the
	// earlier release.
	f.mustHold(securityA, "hold-3")
	f.mustRelease(securityA, "hold-1", "release-1")
	if got, want := holdersOf(f.get()), []string{"user:" + securityA + "/hold-3"}; !cmp.Equal(got, want) {
		t.Errorf("holds after replaying the old release = %v, want %v", got, want)
	}
}

// A held Actor cannot be deleted, so its snapshots stay for investigation.
func TestActorHold_RefusesDelete(t *testing.T) {
	f := newReceiptFixture(t, "ns-hold-delete")
	f.resume()
	f.mustHold(securityA, "hold-1")
	before := f.get()

	_, err := f.tc.client.DeleteActor(f.ctx, &ateapi.DeleteActorRequest{Actor: f.ref})
	wantHeld(t, err)
	f.wantUnchanged(before)

	f.mustRelease(securityA, "hold-1", "release-1")
	if _, err := f.tc.client.DeleteActor(f.ctx, &ateapi.DeleteActorRequest{Actor: f.ref}); err != nil {
		t.Fatalf("DeleteActor after release: %v", err)
	}
}

// An Actor that replaces a held one under the same name is not held, and a
// retry of the old hold returns its receipt without holding the replacement.
func TestActorHold_ReplacementIsNotHeld(t *testing.T) {
	f := newReceiptFixture(t, "ns-hold-replace")
	first := f.mustHold(securityA, "hold-1")
	f.mustRelease(securityA, "hold-1", "release-1")
	if _, err := f.tc.client.DeleteActor(f.ctx, &ateapi.DeleteActorRequest{Actor: f.ref}); err != nil {
		t.Fatalf("DeleteActor: %v", err)
	}
	replacement := f.create()
	if got := replacement.GetStatus().GetHolds(); len(got) != 0 {
		t.Fatalf("replacement holds = %v, want none", got)
	}

	retried := f.mustHold(securityA, "hold-1")
	if diff := cmp.Diff(first.GetReceipt(), retried.GetReceipt(), protocmp.Transform()); diff != "" {
		t.Errorf("retried hold receipt (-original +got):\n%s", diff)
	}
	if retried.GetReceipt().GetActorUid() == replacement.GetMetadata().GetUid() {
		t.Errorf("retried receipt names the replacement")
	}
	if got := f.get().GetStatus().GetHolds(); len(got) != 0 {
		t.Errorf("replacement holds after the retry = %v, want none", got)
	}
	f.resume()

	// A hold conditioned on the old Actor's uid does not hold the
	// replacement.
	_, err := f.hold(securityA, &ateapi.HoldActorRequest{RequestId: "hold-2", ExpectedUid: first.GetReceipt().GetActorUid()})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("hold of the old uid = %v, want FailedPrecondition", err)
	}
	if got := f.get().GetStatus().GetHolds(); len(got) != 0 {
		t.Errorf("replacement holds = %v, want none", got)
	}
}

// A hold conditioned on a run holds only that run.
func TestActorHold_ExpectedActivation(t *testing.T) {
	f := newReceiptFixture(t, "ns-hold-activation")
	stale := f.resume()
	if _, err := f.suspend(&ateapi.SuspendActorRequest{}); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	current := f.resume()
	before := f.get()
	if _, err := f.hold(securityA, &ateapi.HoldActorRequest{RequestId: "hold-stale", ExpectedActivation: stale}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("hold of an earlier run = %v, want FailedPrecondition", err)
	}
	f.wantUnchanged(before)
	resp, err := f.hold(securityA, &ateapi.HoldActorRequest{RequestId: "hold-current", ExpectedActivation: current, IncidentId: "INC-7"})
	if err != nil {
		t.Fatalf("hold of the current run: %v", err)
	}
	r := resp.GetReceipt()
	if r.GetActivationId() != current || r.GetHold().GetIncidentId() != "INC-7" || r.GetState() != ateapi.ActorState_ACTOR_STATE_RUNNING {
		t.Errorf("receipt = %v, want activation %q, incident INC-7, RUNNING", r, current)
	}
}

// Holds stay through RevertActor and through the report that an async
// suspend's upload finished.
func TestActorHold_SurvivesRevertAndUpload(t *testing.T) {
	f := newReceiptFixture(t, "ns-hold-survive")
	f.resume()
	f.mustHold(securityA, "hold-1")
	want := f.get().GetStatus().GetHolds()

	reverted, err := f.tc.client.RevertActor(f.ctx, &ateapi.RevertActorRequest{Actor: f.ref})
	if err != nil {
		t.Fatalf("RevertActor: %v", err)
	}
	if diff := cmp.Diff(want, reverted.GetActor().GetStatus().GetHolds(), protocmp.Transform()); diff != "" {
		t.Errorf("holds after revert (-want +got):\n%s", diff)
	}
	_, err = f.tc.client.ResumeActor(f.ctx, &ateapi.ResumeActorRequest{Actor: f.ref})
	wantHeld(t, err)

	// Run it again, hold it, and suspend it asynchronously.
	f.mustRelease(securityA, "hold-1", "release-1")
	f.resume()
	f.mustHold(securityA, "hold-2")
	want = f.get().GetStatus().GetHolds()
	f.tc.fakeAtelet.Lock.Lock()
	f.tc.fakeAtelet.KeepCheckpointLocal = true
	f.tc.fakeAtelet.Lock.Unlock()
	paused, err := f.suspend(&ateapi.SuspendActorRequest{Options: &ateapi.SuspendOptions{Mode: ateapi.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS}})
	if err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	local := localSnapshot(paused.GetActor().GetStatus())
	if local == nil {
		t.Fatalf("suspend left no local snapshot: %v", paused.GetActor().GetStatus())
	}
	upload := &ateapi.SnapshotReport{
		Uuid:       local.GetUuid(),
		Generation: local.GetGeneration(),
		Fidelity:   local.GetFidelity(),
		Object:     local.GetDurableSnapshot().GetObject(),
	}
	if err := f.tc.service.ApplyActorSnapshotState(f.ctx, resources.ActorRefFromObjectRef(f.ref), paused.GetActor().GetMetadata().GetUid(), "node1", &ateapi.ActorSnapshotState{
		LatestLocal:   upload,
		LatestDurable: upload,
	}); err != nil {
		t.Fatalf("ApplyActorSnapshotState: %v", err)
	}
	after := f.get()
	snaps := after.GetStatus().GetSnapshots()
	if got := snaps[len(snaps)-1].GetDurableSnapshot().GetState(); got != ateapi.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED {
		t.Fatalf("latest durable snapshot = %v, want COMPLETED", got)
	}
	if diff := cmp.Diff(want, after.GetStatus().GetHolds(), protocmp.Transform()); diff != "" {
		t.Errorf("holds after the upload (-want +got):\n%s", diff)
	}
	_, err = f.tc.client.ResumeActor(f.ctx, &ateapi.ResumeActorRequest{Actor: f.ref})
	wantHeld(t, err)
}

// A hold placed while the Actor is being restored stops the resume before
// RUNNING. The restored workload is terminated, and a resume after the
// release restores the Actor under a new activation.
func TestActorHold_DuringRestore(t *testing.T) {
	f := newReceiptFixture(t, "ns-hold-restore")
	// The first resume runs the Actor; the next one restores it.
	f.resume()
	if _, err := f.suspend(&ateapi.SuspendActorRequest{}); err != nil {
		t.Fatalf("SuspendActor: %v", err)
	}
	var holdErr error
	f.tc.fakeAtelet.Lock.Lock()
	f.tc.fakeAtelet.FailTerminate = status.Error(codes.Unavailable, "atelet restarting")
	f.tc.fakeAtelet.RestoreWorkerEpoch = func() int64 {
		f.tc.fakeAtelet.RestoreWorkerEpoch = nil
		_, holdErr = f.hold(securityA, &ateapi.HoldActorRequest{RequestId: "hold-1"})
		return reportedEpoch
	}
	f.tc.fakeAtelet.Lock.Unlock()

	_, err := f.tc.client.ResumeActor(f.ctx, &ateapi.ResumeActorRequest{Actor: f.ref})
	if holdErr != nil {
		t.Fatalf("HoldActor during the restore: %v", holdErr)
	}
	wantHeld(t, err)
	resuming := f.get()
	if got := resuming.GetStatus().GetState(); got != ateapi.ActorState_ACTOR_STATE_RESUMING {
		t.Fatalf("state = %v, want RESUMING", got)
	}
	// The first terminate failed. The next resume the hold refuses tries
	// again.
	f.tc.fakeAtelet.Lock.Lock()
	tried := f.tc.fakeAtelet.TerminateCalled
	f.tc.fakeAtelet.TerminateCalled, f.tc.fakeAtelet.FailTerminate = false, nil
	f.tc.fakeAtelet.Lock.Unlock()
	if !tried {
		t.Fatalf("no terminate was sent for the workload restored for a held Actor")
	}
	_, err = f.tc.client.ResumeActor(f.ctx, &ateapi.ResumeActorRequest{Actor: f.ref})
	wantHeld(t, err)
	f.tc.fakeAtelet.Lock.Lock()
	terminated := f.tc.fakeAtelet.TerminateCalled
	f.tc.fakeAtelet.Lock.Unlock()
	if !terminated {
		t.Errorf("a refused resume of a held RESUMING Actor did not terminate its workload")
	}

	f.mustRelease(securityA, "hold-1", "release-1")
	if got := f.resume(); got == "" || got == resuming.GetStatus().GetActivationId() {
		t.Errorf("activation after release = %q, want a new one (held run %q)", got, resuming.GetStatus().GetActivationId())
	}
}
