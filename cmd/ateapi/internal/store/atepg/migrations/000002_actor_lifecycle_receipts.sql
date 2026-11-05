-- Copyright 2026 Google LLC
--
-- Licensed under the Apache License, Version 2.0 (the "License");
-- you may not use this file except in compliance with the License.
-- You may obtain a copy of the License at
--
--     http://www.apache.org/licenses/LICENSE-2.0
--
-- Unless required by applicable law or agreed to in writing, software
-- distributed under the License is distributed on an "AS IS" BASIS,
-- WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
-- See the License for the specific language governing permissions and
-- limitations under the License.

-- +goose Up

-- One row per SuspendActor or ResumeActor call that set a request_id. Keyed
-- by Actor name, not uid, so a retry after the Actor was replaced still finds
-- the receipt. Rows outlive the Actor and are deleted once expire_time
-- passes, or with the atespace.
CREATE TABLE actor_lifecycle_receipts (
    atespace     text NOT NULL
        REFERENCES atespaces(name) ON DELETE CASCADE,
    actor_name   text NOT NULL,
    request_id   text NOT NULL,
    expire_time  timestamptz NOT NULL,
    proto        bytea NOT NULL,
    PRIMARY KEY (atespace, actor_name, request_id)
);

CREATE INDEX actor_lifecycle_receipts_expire_time
    ON actor_lifecycle_receipts (expire_time);
