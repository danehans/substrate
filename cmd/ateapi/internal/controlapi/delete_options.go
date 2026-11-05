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
	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/cmd/ateapi/internal/store"
	"go.ate.dev/substrate/internal/apierror"
)

// toDeletePreconditions maps opts for a delete of anything but an Actor,
// which has no activation to expect.
func toDeletePreconditions(opts *ateapi.DeleteOptions) (store.DeletePreconditions, error) {
	if opts.GetExpectedActivation() != "" {
		return store.DeletePreconditions{}, apierror.InvalidArgument("options.expected_activation applies only to DeleteActor")
	}
	return store.DeletePreconditions{UID: opts.GetUid(), Version: opts.GetVersion()}, nil
}
