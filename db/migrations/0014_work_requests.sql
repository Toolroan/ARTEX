-- Fulfillment is distinct from delivery. Existing messages are deliberately
-- not backfilled or replayed; only newly committed routing decisions enroll.
CREATE TABLE work_requests (
    id BIGINT PRIMARY KEY REFERENCES main_message_routes(id) ON DELETE CASCADE,
    exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','active','review','blocked','completed','cancelled')),
    acceptance TEXT NOT NULL DEFAULT '',
    resolution TEXT NOT NULL DEFAULT '',
    review_requested BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_work_requests_unresolved ON work_requests(exploration_id,id)
    WHERE state NOT IN ('completed','cancelled');
CREATE TABLE work_request_nodes (
    id BIGSERIAL PRIMARY KEY,
    request_id BIGINT NOT NULL REFERENCES work_requests(id) ON DELETE CASCADE,
    request_revision BIGINT NOT NULL CHECK (request_revision > 0),
    kind TEXT NOT NULL CHECK (kind IN ('goal','intent','evidence')),
    slot_key TEXT NOT NULL,
    summary TEXT NOT NULL,
    allocation_key TEXT NOT NULL,
    creation_hash TEXT NOT NULL DEFAULT '',
    node_id BIGINT REFERENCES exploration_nodes(id) ON DELETE SET NULL,
    observed_terminal_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(request_id,request_revision,kind,allocation_key),
    UNIQUE(request_id,kind,slot_key),
    UNIQUE(request_id,node_id)
);
CREATE INDEX idx_work_request_nodes_node ON work_request_nodes(node_id) WHERE node_id IS NOT NULL;

-- Planner may register only server-granted request goals at runtime. Extend the
-- existing binding while preserving an operator's global disabled setting.
UPDATE tools SET agents=agents || '["planner"]'::jsonb
WHERE key='set_goals' AND NOT (agents ? 'planner');
