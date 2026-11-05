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
	"log/slog"
	"slices"
	"strings"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/apivalidation"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/internal/actorevent"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/ateattr"
	"go.ate.dev/substrate/internal/resources"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// CauseActorQuarantined is the "cause" in the ErrorInfo metadata of a call
// refused because the Actor is quarantined.
const CauseActorQuarantined = "ACTOR_QUARANTINED"

// quarantineSuspendSuffix follows a QuarantineActor request_id to make the
// request_id of the suspend it runs.
const quarantineSuspendSuffix = "/suspend"

// checkNotQuarantined fails with actorQuarantinedError while actor is
// quarantined. doing names the refused call.
func checkNotQuarantined(actorRef resources.ActorRef, actor *ateapi.Actor, doing string) error {
	if actor.GetStatus().GetQuarantine() == nil {
		return nil
	}
	return actorQuarantinedError(actorRef, doing)
}

// actorQuarantinedError is FailedPrecondition with the ErrorInfo reason
// apierror.ReasonTerminalState, which the ingress routers return at once and
// atunnel treats as the end of its certificate renewals.
func actorQuarantinedError(actorRef resources.ActorRef, doing string) error {
	return apierror.FailedPreconditionWithInfo(apierror.ReasonTerminalState, map[string]string{"cause": CauseActorQuarantined},
		"Actor %s is quarantined; %s is refused until the quarantine is released", actorRef, doing)
}

// QuarantineActor quarantines an Actor. The first write places the
// quarantine hold, sets status.quarantine and stores the receipt. A RUNNING
// Actor is then suspended at the activation it held, and the receipt is
// replaced with one that adds QUARANTINE_STEP_SUSPENDED. A retry continues
// an unfinished suspend.
func (s *RPCService) QuarantineActor(ctx context.Context, req *ateapi.QuarantineActorRequest) (*ateapi.QuarantineActorResponse, error) {
	if errs := apivalidation.ValidateQuarantineActorRequest(ctx, req); len(errs) > 0 {
		return nil, resources.ToAPIError(errs)
	}
	actorRef := resources.ActorRefFromObjectRef(req.GetActor())
	setSpanActorRefAttributes(ctx, actorRef)
	caller, err := callerPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	want := &ateapi.LifecycleReceipt{
		RequestId:          req.GetRequestId(),
		Operation:          ateapi.LifecycleOperation_LIFECYCLE_OPERATION_QUARANTINE,
		Actor:              actorRef.ToObjectRef(),
		ExpectedUid:        req.GetExpectedUid(),
		ExpectedActivation: req.GetExpectedActivation(),
		Caller:             caller,
		Reason:             req.GetReason(),
		SuspendRequestId:   req.GetRequestId() + quarantineSuspendSuffix,
		Hold: &ateapi.ActorHold{
			Holder:             store.QuarantineHolder,
			Reason:             req.GetReason(),
			IncidentId:         req.GetIncidentId(),
			RequestId:          req.GetRequestId(),
			ExpectedUid:        req.GetExpectedUid(),
			ExpectedActivation: req.GetExpectedActivation(),
		},
		Quarantine: &ateapi.ActorQuarantine{
			RequestId:  req.GetRequestId(),
			Caller:     caller,
			Reason:     req.GetReason(),
			IncidentId: req.GetIncidentId(),
		},
	}
	receipt, err := s.storedQuarantine(ctx, actorRef, want)
	if err != nil {
		return nil, err
	}
	var actor *ateapi.Actor
	if receipt == nil {
		if actor, receipt, err = s.placeQuarantine(ctx, actorRef, want); err != nil {
			return nil, err
		}
	}
	if !quarantineSuspendPending(receipt) {
		return &ateapi.QuarantineActorResponse{Actor: actor, Receipt: receipt}, nil
	}
	// A retry continues the suspend only while its quarantine is in place.
	if actor == nil {
		current, err := s.impl.GetActor(ctx, actorRef)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		if current.GetStatus().GetQuarantine().GetRequestId() != receipt.GetRequestId() || current.GetMetadata().GetUid() != receipt.GetActorUid() {
			return &ateapi.QuarantineActorResponse{Receipt: receipt}, nil
		}
	}
	suspended, final, err := s.finishQuarantineSuspend(ctx, actorRef, receipt)
	if err != nil {
		return nil, err
	}
	if suspended != nil {
		setSpanActorAttributes(ctx, suspended)
	}
	return &ateapi.QuarantineActorResponse{Actor: suspended, Receipt: final}, nil
}

// placeQuarantine places the quarantine hold, sets status.quarantine and
// stores the receipt, in one write.
func (s *RPCService) placeQuarantine(ctx context.Context, actorRef resources.ActorRef, want *ateapi.LifecycleReceipt) (*ateapi.Actor, *ateapi.LifecycleReceipt, error) {
	cond := lifecycleConditions{expectedUID: want.GetExpectedUid(), expectedActivation: want.GetExpectedActivation(), requestID: want.GetRequestId()}
	actor, receipt, err := s.impl.PlaceActorHold(ctx, actorRef, func(actor *ateapi.Actor) error {
		if actor.GetMetadata().GetDeleteTime() != nil {
			return apierror.FailedPrecondition("Actor %s is being deleted", actorRef)
		}
		if err := cond.check(actorRef, actor); err != nil {
			return err
		}
		if q := actor.GetStatus().GetQuarantine(); q.GetRequestId() == want.GetRequestId() {
			// A concurrent call with this request_id placed it first.
			return store.ErrAlreadyExists
		} else if q != nil {
			return apierror.AlreadyExists("Actor %s is already quarantined under request_id %q", actorRef, q.GetRequestId())
		}
		// A revert under way discards the snapshots a quarantine keeps.
		if actor.GetStatus().GetState() == ateapi.ActorState_ACTOR_STATE_REVERTING {
			return apierror.FailedPrecondition("Actor %s is being reverted, please retry once it is SUSPENDED", actorRef)
		}
		for _, h := range actor.GetStatus().GetHolds() {
			if h.GetRequestId() == want.GetRequestId() {
				return apierror.AlreadyExists("another hold on Actor %s has request_id %q", actorRef, want.GetRequestId())
			}
		}
		if len(actor.GetStatus().GetHolds()) >= maxActorHolds {
			return apierror.FailedPrecondition("Actor %s already has %d holds", actorRef, maxActorHolds)
		}
		return nil
	}, func(stored *ateapi.Actor) *ateapi.LifecycleReceipt {
		r := proto.CloneOf(want)
		r.ActorUid = stored.GetMetadata().GetUid()
		r.ActivationId = stored.GetStatus().GetActivationId()
		r.Quarantine.ActivationId = r.ActivationId
		r.Transitioned = true
		r.State = stored.GetStatus().GetState()
		r.Steps = []ateapi.QuarantineStep{ateapi.QuarantineStep_QUARANTINE_STEP_HOLD, ateapi.QuarantineStep_QUARANTINE_STEP_MARKED}
		return r
	})
	if errors.Is(err, store.ErrAlreadyExists) {
		// Another call stored the quarantine or the receipt first.
		stored, err := s.storedQuarantine(ctx, actorRef, want)
		if stored != nil || err != nil {
			return nil, stored, err
		}
		return nil, nil, apierror.Aborted("concurrent quarantine of Actor %s, please retry", actorRef)
	}
	if err != nil {
		return nil, nil, actorWriteError(actorRef, err)
	}
	setSpanActorAttributes(ctx, actor)
	logQuarantineChanged(ctx, actor, ateattr.QuarantineActionQuarantined, receipt, "")
	return actor, receipt, nil
}

// quarantineSuspendPending reports whether the quarantine r records found
// the Actor RUNNING and has not recorded its suspend.
func quarantineSuspendPending(r *ateapi.LifecycleReceipt) bool {
	return r.GetState() == ateapi.ActorState_ACTOR_STATE_RUNNING && r.GetActivationId() != "" &&
		!slices.Contains(r.GetSteps(), ateapi.QuarantineStep_QUARANTINE_STEP_SUSPENDED)
}

// finishQuarantineSuspend suspends the activation r records, under r's
// suspend_request_id, and replaces r with the finished receipt. An Actor
// that no longer holds that activation, because it crashed or another call
// suspended it, finishes the receipt without QUARANTINE_STEP_SUSPENDED. If
// the Actor r names is gone, r is returned as it is, as a lifecycle receipt
// is once its Actor was replaced. Returns the suspended Actor, or nil if the
// suspend replayed its receipt or found nothing to suspend.
func (s *RPCService) finishQuarantineSuspend(ctx context.Context, actorRef resources.ActorRef, r *ateapi.LifecycleReceipt) (*ateapi.Actor, *ateapi.LifecycleReceipt, error) {
	cond := lifecycleConditions{expectedUID: r.GetActorUid(), expectedActivation: r.GetActivationId(), requestID: r.GetSuspendRequestId()}
	final := proto.CloneOf(r)
	actor, suspend, err := s.actorWorkflow.SuspendActor(ctx, actorRef, suspendAsync, cond)
	// A replayed receipt of a suspend that never finished is in SUSPENDING.
	if err == nil && suspend.GetTransitioned() && suspend.GetActivationId() == r.GetActivationId() &&
		suspend.GetState() == ateapi.ActorState_ACTOR_STATE_SUSPENDED {
		final.State = suspend.GetState()
		final.Steps = append(final.Steps, ateapi.QuarantineStep_QUARANTINE_STEP_SUSPENDED)
		final.Mode = suspend.GetMode()
		final.SnapshotDurable = suspend.GetSnapshotDurable()
	} else {
		if err != nil && status.Code(err) != codes.FailedPrecondition && !errors.Is(err, store.ErrNotFound) {
			return nil, nil, err
		}
		// The suspend did not stop the activation. Finish the receipt only
		// if the Actor no longer holds it.
		current, getErr := s.impl.GetActor(ctx, actorRef)
		switch {
		case errors.Is(getErr, store.ErrNotFound) || getErr == nil && current.GetMetadata().GetUid() != r.GetActorUid():
			return nil, r, nil
		case getErr != nil:
			return nil, nil, getErr
		case err != nil && current.GetStatus().GetActivationId() == r.GetActivationId():
			return nil, nil, err
		}
		actor = nil
		final.State = current.GetStatus().GetState()
	}
	if err := s.impl.ReplaceActorHoldReceipt(ctx, actorRef, final); err != nil {
		return nil, nil, err
	}
	if actor != nil && slices.Contains(final.GetSteps(), ateapi.QuarantineStep_QUARANTINE_STEP_SUSPENDED) {
		logQuarantineChanged(ctx, actor, ateattr.QuarantineActionSuspended, final, "")
	}
	return actor, final, nil
}

// storedQuarantine returns the receipt a QuarantineActor call like want
// already stored, from the receipts or from the quarantine hold, which
// outlives the receipt. It fails with AlreadyExists if the request_id was
// used with other parameters.
func (s *RPCService) storedQuarantine(ctx context.Context, actorRef resources.ActorRef, want *ateapi.LifecycleReceipt) (*ateapi.LifecycleReceipt, error) {
	stored, err := s.impl.GetActorLifecycleReceipt(ctx, actorRef, want.GetRequestId())
	if errors.Is(err, store.ErrNotFound) {
		stored, err = s.impl.GetActorHold(ctx, actorRef, store.QuarantineHolder)
		if errors.Is(err, store.ErrNotFound) || err == nil && stored.GetRequestId() != want.GetRequestId() {
			return nil, nil
		}
	}
	if err != nil {
		return nil, err
	}
	if !sameQuarantineCall(stored, want) {
		return nil, apierror.AlreadyExists("request_id %q was used for Actor %s with different parameters", want.GetRequestId(), actorRef)
	}
	return stored, nil
}

// sameQuarantineCall reports whether stored records a QuarantineActor or
// ReleaseQuarantine call with want's caller and parameters. A release names
// no incident, so its receipt's incident is the released quarantine's.
func sameQuarantineCall(stored, want *ateapi.LifecycleReceipt) bool {
	return stored.GetOperation() == want.GetOperation() &&
		stored.GetExpectedUid() == want.GetExpectedUid() &&
		stored.GetExpectedActivation() == want.GetExpectedActivation() &&
		stored.GetCaller() == want.GetCaller() &&
		stored.GetReason() == want.GetReason() &&
		stored.GetHold().GetRequestId() == want.GetHold().GetRequestId() &&
		(want.GetOperation() != ateapi.LifecycleOperation_LIFECYCLE_OPERATION_QUARANTINE ||
			stored.GetHold().GetIncidentId() == want.GetHold().GetIncidentId())
}

// ReleaseQuarantine releases the quarantine named by quarantine_request_id.
// It does not resume the Actor. A release that finds no such quarantine
// stores a receipt with transitioned false.
func (s *RPCService) ReleaseQuarantine(ctx context.Context, req *ateapi.ReleaseQuarantineRequest) (*ateapi.ReleaseQuarantineResponse, error) {
	if errs := apivalidation.ValidateReleaseQuarantineRequest(ctx, req); len(errs) > 0 {
		return nil, resources.ToAPIError(errs)
	}
	actorRef := resources.ActorRefFromObjectRef(req.GetActor())
	setSpanActorRefAttributes(ctx, actorRef)
	caller, err := callerPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	want := &ateapi.LifecycleReceipt{
		RequestId:   req.GetRequestId(),
		Operation:   ateapi.LifecycleOperation_LIFECYCLE_OPERATION_RELEASE_QUARANTINE,
		Actor:       actorRef.ToObjectRef(),
		ExpectedUid: req.GetExpectedUid(),
		Caller:      caller,
		Reason:      req.GetReason(),
		Hold:        &ateapi.ActorHold{Holder: store.QuarantineHolder, RequestId: req.GetQuarantineRequestId()},
		Quarantine:  &ateapi.ActorQuarantine{RequestId: req.GetQuarantineRequestId()},
	}
	stored, err := s.impl.GetActorLifecycleReceipt(ctx, actorRef, req.GetRequestId())
	switch {
	case err == nil && !sameQuarantineCall(stored, want):
		return nil, apierror.AlreadyExists("request_id %q was used for Actor %s with different parameters", req.GetRequestId(), actorRef)
	case err == nil:
		return &ateapi.ReleaseQuarantineResponse{Receipt: stored}, nil
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}

	cond := lifecycleConditions{expectedUID: req.GetExpectedUid(), requestID: req.GetRequestId()}
	// released is the quarantine as the locked Actor recorded it.
	var released *ateapi.ActorQuarantine
	actor, receipt, err := s.impl.ReleaseActorHold(ctx, actorRef, store.QuarantineHolder, req.GetQuarantineRequestId(), func(actor *ateapi.Actor) error {
		if err := cond.check(actorRef, actor); err != nil {
			return err
		}
		if q := actor.GetStatus().GetQuarantine(); q.GetRequestId() == req.GetQuarantineRequestId() {
			released = proto.CloneOf(q)
		}
		return nil
	}, func(stored *ateapi.Actor, hold *ateapi.ActorHold) *ateapi.LifecycleReceipt {
		r := proto.CloneOf(want)
		r.ActorUid = stored.GetMetadata().GetUid()
		r.ActivationId = stored.GetStatus().GetActivationId()
		r.State = stored.GetStatus().GetState()
		if hold != nil {
			r.Hold = proto.CloneOf(hold)
			r.Quarantine = proto.CloneOf(released)
			r.Transitioned = true
		}
		return r
	})
	if errors.Is(err, store.ErrAlreadyExists) {
		return nil, apierror.Aborted("another call stored a receipt for request_id %q first, please retry", req.GetRequestId())
	}
	if err != nil {
		return nil, actorWriteError(actorRef, err)
	}
	setSpanActorAttributes(ctx, actor)
	if receipt.GetTransitioned() {
		logQuarantineChanged(ctx, actor, ateattr.QuarantineActionReleased, receipt, req.GetRequestId())
	}
	return &ateapi.ReleaseQuarantineResponse{Actor: actor, Receipt: receipt}, nil
}

// logResumeRefusedByQuarantine audits a resume refused because actor is
// quarantined. The caller is empty for an in-process resume, such as the
// template reconciler's.
func logResumeRefusedByQuarantine(ctx context.Context, actor *ateapi.Actor) {
	q := actor.GetStatus().GetQuarantine()
	if q == nil {
		return
	}
	caller, _ := callerPrincipal(ctx)
	r := &ateapi.LifecycleReceipt{
		ActivationId: q.GetActivationId(),
		Caller:       caller,
		Reason:       q.GetReason(),
		Hold:         &ateapi.ActorHold{RequestId: q.GetRequestId(), IncidentId: q.GetIncidentId()},
	}
	logQuarantineChanged(ctx, actor, ateattr.QuarantineActionResumeRefused, r, "")
}

// logQuarantineChanged writes the audit record of a quarantine action. r is
// the receipt of the call, or for a refused resume the quarantine it hit.
// releaseRequestID is set only for a release.
func logQuarantineChanged(ctx context.Context, actor *ateapi.Actor, action string, r *ateapi.LifecycleReceipt, releaseRequestID string) {
	attribution := resources.ActorAttributionFromActor(actor)
	// The activation the quarantine found, which a suspend has since cleared.
	attribution.ActivationID = r.GetActivationId()
	if action == ateattr.QuarantineActionReleased {
		attribution.ActivationID = r.GetQuarantine().GetActivationId()
	}
	attrs := ateattr.ActorLogAttrs(attribution)
	attrs = append(attrs,
		slog.String(string(ateattr.ActorQuarantineActionKey), action),
		slog.String(string(ateattr.ActorQuarantineRequestIDKey), r.GetHold().GetRequestId()),
		slog.String(string(ateattr.ActorQuarantineReasonKey), r.GetReason()),
		slog.String(string(ateattr.ActorStateKey), ateattr.ActorStateValue(actor.GetStatus().GetState())))
	if r.GetCaller() != "" {
		attrs = append(attrs, slog.String(string(ateattr.ActorQuarantineCallerKey), r.GetCaller()))
	}
	if id := r.GetHold().GetIncidentId(); id != "" {
		attrs = append(attrs, slog.String(string(ateattr.ActorQuarantineIncidentIDKey), id))
	}
	if len(r.GetSteps()) > 0 {
		attrs = append(attrs, slog.String(string(ateattr.ActorQuarantineStepsKey), quarantineStepsValue(r.GetSteps())))
	}
	if releaseRequestID != "" {
		attrs = append(attrs, slog.String(string(ateattr.ActorQuarantineReleaseRequestIDKey), releaseRequestID))
	}
	actorevent.Log(ctx, actorevent.QuarantineChanged, attrs)
}

// quarantineStepsValue joins steps as "hold,marked,suspended".
func quarantineStepsValue(steps []ateapi.QuarantineStep) string {
	names := make([]string, len(steps))
	for i, step := range steps {
		names[i] = strings.ToLower(strings.TrimPrefix(step.String(), "QUARANTINE_STEP_"))
	}
	return strings.Join(names, ",")
}
