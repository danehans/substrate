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

package controlapi

import (
	"context"
	"errors"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/resources"
	"google.golang.org/protobuf/proto"
)

// lifecycleConditions are the optional expected_uid, expected_activation and
// request_id of a SuspendActor or ResumeActor call.
type lifecycleConditions struct {
	expectedUID        string
	expectedActivation string
	requestID          string
}

// check fails with FailedPrecondition unless actor has the expected uid and
// activation.
func (c lifecycleConditions) check(actorRef resources.ActorRef, actor *ateapi.Actor) error {
	if c.expectedUID != "" && actor.GetMetadata().GetUid() != c.expectedUID {
		return apierror.FailedPrecondition("Actor %s has uid %q, not the expected %q", actorRef, actor.GetMetadata().GetUid(), c.expectedUID)
	}
	if c.expectedActivation != "" && actor.GetStatus().GetActivationId() != c.expectedActivation {
		return apierror.FailedPrecondition("Actor %s holds activation %q, not the expected %q", actorRef, actor.GetStatus().GetActivationId(), c.expectedActivation)
	}
	return nil
}

// lifecycleCall carries a call's conditions and receipt through the workflow
// steps that write its transition.
type lifecycleCall struct {
	actorRef resources.ActorRef
	cond     lifecycleConditions
	// receipt holds the fields every receipt of the call shares. Nil without a
	// request_id.
	receipt *ateapi.LifecycleReceipt
	// continues is set when the call found its own unfinished receipt, so it
	// continues that transition without checking the conditions again.
	continues bool
	// stored is set once a receipt for the call is stored.
	stored bool
}

func newLifecycleCall(actorRef resources.ActorRef, cond lifecycleConditions, op ateapi.LifecycleOperation, mode ateapi.DurabilityMode) *lifecycleCall {
	c := &lifecycleCall{actorRef: actorRef, cond: cond}
	if cond.requestID != "" {
		c.receipt = &ateapi.LifecycleReceipt{
			RequestId:          cond.requestID,
			Operation:          op,
			Actor:              &ateapi.ObjectRef{Atespace: actorRef.Atespace, Name: actorRef.Name},
			ExpectedUid:        cond.expectedUID,
			ExpectedActivation: cond.expectedActivation,
			Mode:               mode,
		}
	}
	return c
}

// lookup returns the receipt stored for the call's request_id, or nil if
// there is none. A receipt in inProgress marks the call as continuing it; a
// receipt in any other state is final. Reuse of the request_id with other
// parameters fails with AlreadyExists.
func (c *lifecycleCall) lookup(ctx context.Context, st actorWorkflowStore, inProgress ateapi.ActorState) (_ *ateapi.LifecycleReceipt, err error) {
	if c.receipt == nil {
		return nil, nil
	}
	stored, err := st.GetActorLifecycleReceipt(ctx, c.actorRef, c.cond.requestID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if stored.GetOperation() != c.receipt.GetOperation() ||
		stored.GetExpectedUid() != c.receipt.GetExpectedUid() ||
		stored.GetExpectedActivation() != c.receipt.GetExpectedActivation() ||
		stored.GetMode() != c.receipt.GetMode() {
		return nil, apierror.AlreadyExists("request_id %q was used for Actor %s with different parameters", c.cond.requestID, c.actorRef)
	}
	if stored.GetState() == inProgress {
		c.continues, c.stored = true, true
	}
	return stored, nil
}

// resumes reports whether actor is still in the transition receipt records,
// under the same uid and activation, so the call can finish it.
func resumes(receipt *ateapi.LifecycleReceipt, actor *ateapi.Actor) bool {
	return actor.GetMetadata().GetUid() == receipt.GetActorUid() &&
		actor.GetStatus().GetState() == receipt.GetState() &&
		actor.GetStatus().GetActivationId() == receipt.GetActivationId()
}

// checkInWrite checks the conditions inside the write that commits the
// transition. A call that continues its own receipt checked them when it
// started.
func (c *lifecycleCall) checkInWrite(actor *ateapi.Actor) error {
	if c == nil || c.continues {
		return nil
	}
	return c.cond.check(c.actorRef, actor)
}

// updateActor is store.UpdateActor that, for a call with a request_id, also
// stores the call's receipt in the same transaction. fill completes the
// receipt from the Actor as stored. A nil mutate stores only the receipt. A
// nil call has no receipt.
func (c *lifecycleCall) updateActor(ctx context.Context, st actorWorkflowStore, actorRef resources.ActorRef, precondition store.Precondition, mutate func(*ateapi.Actor) error, fill func(stored *ateapi.Actor, r *ateapi.LifecycleReceipt)) (*ateapi.Actor, *ateapi.LifecycleReceipt, error) {
	return c.write(func() (*ateapi.Actor, error) {
		return st.UpdateActor(ctx, actorRef, precondition, mutate)
	}, func(receipt func(*ateapi.Actor) *ateapi.LifecycleReceipt, create bool) (*ateapi.Actor, *ateapi.LifecycleReceipt, error) {
		return st.UpdateActorWithReceipt(ctx, actorRef, precondition, mutate, receipt, create)
	}, fill)
}

// updateActorAndRelease is store.UpdateActorAndRelease with the call's
// receipt, as updateActor stores it.
func (c *lifecycleCall) updateActorAndRelease(ctx context.Context, st actorWorkflowStore, actorRef resources.ActorRef, precondition store.Precondition, mutate func(*ateapi.Actor) error, fill func(stored *ateapi.Actor, r *ateapi.LifecycleReceipt)) (*ateapi.Actor, *ateapi.Worker, *ateapi.LifecycleReceipt, error) {
	var released *ateapi.Worker
	stored, receipt, err := c.write(func() (*ateapi.Actor, error) {
		actor, worker, err := st.UpdateActorAndRelease(ctx, actorRef, precondition, mutate)
		released = worker
		return actor, err
	}, func(receipt func(*ateapi.Actor) *ateapi.LifecycleReceipt, create bool) (*ateapi.Actor, *ateapi.LifecycleReceipt, error) {
		actor, worker, storedReceipt, err := st.UpdateActorAndReleaseWithReceipt(ctx, actorRef, precondition, mutate, receipt, create)
		released = worker
		return actor, storedReceipt, err
	}, fill)
	return stored, released, receipt, err
}

// bindActorToWorker is store.BindActorToWorker with the call's receipt, as
// updateActor stores it.
func (c *lifecycleCall) bindActorToWorker(ctx context.Context, st actorWorkflowStore, workerName string, assignment *ateapi.ActorAssignment, admit func(*ateapi.Worker) error, actorRef resources.ActorRef, precondition store.Precondition, mutate func(*ateapi.Actor) error, fill func(stored *ateapi.Actor, r *ateapi.LifecycleReceipt)) (*ateapi.Actor, *ateapi.LifecycleReceipt, error) {
	return c.write(func() (*ateapi.Actor, error) {
		return st.BindActorToWorker(ctx, workerName, assignment, admit, actorRef, precondition, mutate)
	}, func(receipt func(*ateapi.Actor) *ateapi.LifecycleReceipt, create bool) (*ateapi.Actor, *ateapi.LifecycleReceipt, error) {
		return st.BindActorToWorkerWithReceipt(ctx, workerName, assignment, admit, actorRef, precondition, mutate, receipt, create)
	}, fill)
}

// write runs plain for a call without a request_id, and otherwise
// withReceipt, passing it the call's receipt.
func (c *lifecycleCall) write(plain func() (*ateapi.Actor, error), withReceipt func(receipt func(*ateapi.Actor) *ateapi.LifecycleReceipt, create bool) (*ateapi.Actor, *ateapi.LifecycleReceipt, error), fill func(stored *ateapi.Actor, r *ateapi.LifecycleReceipt)) (*ateapi.Actor, *ateapi.LifecycleReceipt, error) {
	if c == nil || c.receipt == nil {
		stored, err := plain()
		return stored, nil, err
	}
	stored, r, err := withReceipt(func(stored *ateapi.Actor) *ateapi.LifecycleReceipt {
		r := proto.CloneOf(c.receipt)
		r.ActorUid = stored.GetMetadata().GetUid()
		fill(stored, r)
		return r
	}, !c.stored)
	if errors.Is(err, store.ErrAlreadyExists) {
		return nil, nil, apierror.Aborted("another call stored a receipt for request_id %q first, please retry", c.cond.requestID)
	}
	if err != nil {
		return nil, nil, err
	}
	c.stored = true
	return stored, r, nil
}

// checkActorExecution rejects an observation from another actor or activation,
// including a cleared activation. Unlike request conditions, empty is exact.
func checkActorExecution(ref resources.ActorRef, expected, current *ateapi.Actor) error {
	if expected.GetMetadata().GetUid() != current.GetMetadata().GetUid() ||
		expected.GetStatus().GetActivationId() != current.GetStatus().GetActivationId() {
		return apierror.Aborted("actor %s changed execution", ref)
	}
	return nil
}
