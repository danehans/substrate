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

package controlapi

import (
	"context"
	"errors"
	"log/slog"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/apivalidation"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/internal/actorevent"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/ateattr"
	"go.ate.dev/substrate/internal/principal"
	"go.ate.dev/substrate/internal/resources"
	"google.golang.org/protobuf/proto"
)

// maxActorHolds is the maxItems of ActorStatus.holds.
const maxActorHolds = 16

// CauseActorHeld is the "cause" in the ErrorInfo metadata of a call refused
// because the Actor is held.
const CauseActorHeld = "ACTOR_HELD"

// errHeldAtWrite is the error of the RUNNING write when a hold was placed
// during the restore. ResumeActor returns actorHeldError in its place.
var errHeldAtWrite = errors.New("actor was held during its restore")

// checkNotHeld fails with actorHeldError while actor has a hold.
func checkNotHeld(actorRef resources.ActorRef, actor *ateapi.Actor) error {
	if len(actor.GetStatus().GetHolds()) == 0 {
		return nil
	}
	return actorHeldError(actorRef)
}

// actorHeldError is FailedPrecondition with the ErrorInfo reason
// apierror.ReasonTerminalState, so the ingress router fails the request at
// once instead of retrying it.
func actorHeldError(actorRef resources.ActorRef) error {
	return apierror.FailedPreconditionWithInfo(apierror.ReasonTerminalState, map[string]string{"cause": CauseActorHeld},
		"Actor %s is held until every hold on it is released", actorRef)
}

// callerPrincipal names the authenticated caller as AccessPolicy members do.
func callerPrincipal(ctx context.Context) (string, error) {
	p, ok := principal.FromContext(ctx)
	if !ok || p.ID == "" {
		return "", apierror.Unauthenticated("unauthenticated: missing principal in context")
	}
	return "user:" + p.ID, nil
}

// HoldActor places the caller's hold on an Actor. It takes no lease: the
// write locks the Actor row and advances its version, so a resume that read
// the Actor before the hold fails its own write and reads the hold on retry.
func (s *RPCService) HoldActor(ctx context.Context, req *ateapi.HoldActorRequest) (*ateapi.HoldActorResponse, error) {
	if errs := apivalidation.ValidateHoldActorRequest(ctx, req); len(errs) > 0 {
		return nil, resources.ToAPIError(errs)
	}
	actorRef := resources.ActorRefFromObjectRef(req.GetActor())
	setSpanActorRefAttributes(ctx, actorRef)
	holder, err := callerPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	cond := lifecycleConditions{expectedUID: req.GetExpectedUid(), expectedActivation: req.GetExpectedActivation(), requestID: req.GetRequestId()}
	want := &ateapi.LifecycleReceipt{
		RequestId:          req.GetRequestId(),
		Operation:          ateapi.LifecycleOperation_LIFECYCLE_OPERATION_HOLD,
		Actor:              actorRef.ToObjectRef(),
		ExpectedUid:        req.GetExpectedUid(),
		ExpectedActivation: req.GetExpectedActivation(),
		Hold: &ateapi.ActorHold{
			Holder:             holder,
			Reason:             req.GetReason(),
			IncidentId:         req.GetIncidentId(),
			RequestId:          req.GetRequestId(),
			ExpectedUid:        req.GetExpectedUid(),
			ExpectedActivation: req.GetExpectedActivation(),
		},
	}
	if stored, err := s.storedHold(ctx, actorRef, want); stored != nil || err != nil {
		return &ateapi.HoldActorResponse{Receipt: stored}, err
	}

	held, receipt, err := s.impl.PlaceActorHold(ctx, actorRef, func(actor *ateapi.Actor) error {
		if actor.GetMetadata().GetDeleteTime() != nil {
			return apierror.FailedPrecondition("Actor %s is being deleted", actorRef)
		}
		if err := cond.check(actorRef, actor); err != nil {
			return err
		}
		for _, h := range actor.GetStatus().GetHolds() {
			if h.GetRequestId() == req.GetRequestId() {
				return apierror.AlreadyExists("another hold on Actor %s has request_id %q", actorRef, req.GetRequestId())
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
		r.Transitioned = true
		r.State = stored.GetStatus().GetState()
		return r
	})
	if errors.Is(err, store.ErrAlreadyExists) {
		// Another call stored the hold or the receipt first.
		if stored, err := s.storedHold(ctx, actorRef, want); stored != nil || err != nil {
			return &ateapi.HoldActorResponse{Receipt: stored}, err
		}
		return nil, apierror.Aborted("concurrent hold on Actor %s, please retry", actorRef)
	}
	if err != nil {
		return nil, actorUpdateError(actorRef, err)
	}
	setSpanActorAttributes(ctx, held)
	logHoldChanged(ctx, held, ateattr.HoldActionPlaced, holder, receipt.GetHold(), "")
	return &ateapi.HoldActorResponse{Actor: held, Receipt: receipt}, nil
}

// storedHold returns the receipt a HoldActor call like want already stored,
// from the receipts or from the hold it placed, which outlives the receipt.
// It fails with AlreadyExists if the request_id was used with other
// parameters, or if the caller already has another hold on the Actor.
func (s *RPCService) storedHold(ctx context.Context, actorRef resources.ActorRef, want *ateapi.LifecycleReceipt) (*ateapi.LifecycleReceipt, error) {
	stored, err := s.storedReceipt(ctx, actorRef, want)
	if stored != nil || err != nil {
		return stored, err
	}
	placed, err := s.impl.GetActorHold(ctx, actorRef, want.GetHold().GetHolder())
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if placed.GetRequestId() != want.GetRequestId() {
		return nil, apierror.AlreadyExists("the caller already holds Actor %s under request_id %q", actorRef, placed.GetRequestId())
	}
	if !sameHoldCall(placed, want) {
		return nil, apierror.AlreadyExists("request_id %q was used for Actor %s with different parameters", want.GetRequestId(), actorRef)
	}
	return placed, nil
}

// storedReceipt returns the unexpired receipt stored for want's request_id,
// or nil if there is none. It fails with AlreadyExists if that receipt
// records another call.
func (s *RPCService) storedReceipt(ctx context.Context, actorRef resources.ActorRef, want *ateapi.LifecycleReceipt) (*ateapi.LifecycleReceipt, error) {
	stored, err := s.impl.GetActorLifecycleReceipt(ctx, actorRef, want.GetRequestId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !sameHoldCall(stored, want) {
		return nil, apierror.AlreadyExists("request_id %q was used for Actor %s with different parameters", want.GetRequestId(), actorRef)
	}
	return stored, nil
}

// sameHoldCall reports whether stored records a call with want's caller and
// parameters.
func sameHoldCall(stored, want *ateapi.LifecycleReceipt) bool {
	sh, wh := stored.GetHold(), want.GetHold()
	return stored.GetOperation() == want.GetOperation() &&
		stored.GetExpectedUid() == want.GetExpectedUid() &&
		stored.GetExpectedActivation() == want.GetExpectedActivation() &&
		sh.GetHolder() == wh.GetHolder() &&
		sh.GetRequestId() == wh.GetRequestId() &&
		(want.GetOperation() != ateapi.LifecycleOperation_LIFECYCLE_OPERATION_HOLD ||
			sh.GetReason() == wh.GetReason() && sh.GetIncidentId() == wh.GetIncidentId())
}

// ReleaseActorHold releases the caller's hold named by hold_request_id. A
// hold another principal placed is never released. A release that finds no
// hold of the caller's stores a receipt with transitioned false.
func (s *RPCService) ReleaseActorHold(ctx context.Context, req *ateapi.ReleaseActorHoldRequest) (*ateapi.ReleaseActorHoldResponse, error) {
	if errs := apivalidation.ValidateReleaseActorHoldRequest(ctx, req); len(errs) > 0 {
		return nil, resources.ToAPIError(errs)
	}
	actorRef := resources.ActorRefFromObjectRef(req.GetActor())
	setSpanActorRefAttributes(ctx, actorRef)
	holder, err := callerPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	cond := lifecycleConditions{expectedUID: req.GetExpectedUid(), requestID: req.GetRequestId()}
	want := &ateapi.LifecycleReceipt{
		RequestId:   req.GetRequestId(),
		Operation:   ateapi.LifecycleOperation_LIFECYCLE_OPERATION_RELEASE_HOLD,
		Actor:       actorRef.ToObjectRef(),
		ExpectedUid: req.GetExpectedUid(),
		Hold:        &ateapi.ActorHold{Holder: holder, RequestId: req.GetHoldRequestId()},
	}
	if stored, err := s.storedReceipt(ctx, actorRef, want); stored != nil || err != nil {
		return &ateapi.ReleaseActorHoldResponse{Receipt: stored}, err
	}

	actor, receipt, err := s.impl.ReleaseActorHold(ctx, actorRef, holder, req.GetHoldRequestId(), func(actor *ateapi.Actor) error {
		if err := cond.check(actorRef, actor); err != nil {
			return err
		}
		for _, h := range actor.GetStatus().GetHolds() {
			if h.GetRequestId() == req.GetHoldRequestId() && h.GetHolder() != holder {
				return apierror.PermissionDenied("hold %q on Actor %s was placed by another principal", req.GetHoldRequestId(), actorRef)
			}
		}
		return nil
	}, func(stored *ateapi.Actor, released *ateapi.ActorHold) *ateapi.LifecycleReceipt {
		r := proto.CloneOf(want)
		r.ActorUid = stored.GetMetadata().GetUid()
		r.ActivationId = stored.GetStatus().GetActivationId()
		r.State = stored.GetStatus().GetState()
		if released != nil {
			r.Hold = proto.CloneOf(released)
			r.Transitioned = true
		}
		return r
	})
	if errors.Is(err, store.ErrAlreadyExists) {
		return nil, apierror.Aborted("another call stored a receipt for request_id %q first, please retry", req.GetRequestId())
	}
	if err != nil {
		return nil, actorUpdateError(actorRef, err)
	}
	setSpanActorAttributes(ctx, actor)
	if receipt.GetTransitioned() {
		logHoldChanged(ctx, actor, ateattr.HoldActionReleased, holder, receipt.GetHold(), req.GetRequestId())
	}
	return &ateapi.ReleaseActorHoldResponse{Actor: actor, Receipt: receipt}, nil
}

// logHoldChanged writes the audit record of a hold placed or released by
// caller. releaseRequestID is empty for a hold.
func logHoldChanged(ctx context.Context, actor *ateapi.Actor, action, caller string, hold *ateapi.ActorHold, releaseRequestID string) {
	attrs := ateattr.ActorLogAttrs(resources.ActorAttributionFromActor(actor))
	attrs = append(attrs,
		slog.String(string(ateattr.ActorHoldActionKey), action),
		slog.String(string(ateattr.ActorHoldHolderKey), hold.GetHolder()),
		slog.String(string(ateattr.ActorHoldCallerKey), caller),
		slog.String(string(ateattr.ActorHoldReasonKey), hold.GetReason()),
		slog.String(string(ateattr.ActorHoldRequestIDKey), hold.GetRequestId()),
		slog.String(string(ateattr.ActorStateKey), ateattr.ActorStateValue(actor.GetStatus().GetState())))
	if id := hold.GetIncidentId(); id != "" {
		attrs = append(attrs, slog.String(string(ateattr.ActorHoldIncidentIDKey), id))
	}
	if releaseRequestID != "" {
		attrs = append(attrs, slog.String(string(ateattr.ActorHoldReleaseRequestIDKey), releaseRequestID))
	}
	actorevent.Log(ctx, actorevent.HoldChanged, attrs)
}
