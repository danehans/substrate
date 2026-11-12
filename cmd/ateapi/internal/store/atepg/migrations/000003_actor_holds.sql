-- Copyright 2026 The Agent Substrate Authors
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

-- One row per hold on an Actor, with the receipt of the HoldActor call that
-- placed it. Each holder has at most one hold on an Actor, and no two holds
-- on an Actor share a request_id. The Actor's
-- status.holds lists the same holds and is written in the same transaction.
-- The foreign key refuses to delete the row of a held Actor.
CREATE TABLE actor_holds (
    atespace    text NOT NULL,
    actor_name  text NOT NULL,
    holder      text NOT NULL,
    request_id  text NOT NULL,
    receipt     bytea NOT NULL,
    PRIMARY KEY (atespace, actor_name, holder),
    UNIQUE (atespace, actor_name, request_id),
    FOREIGN KEY (atespace, actor_name)
        REFERENCES actors(atespace, name) ON DELETE RESTRICT
);
