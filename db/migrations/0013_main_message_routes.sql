-- Conversation context is separate from actionable Planner instructions. No
-- legacy hints or activity are reclassified or replayed by this migration.
CREATE TABLE main_message_routes (
    id BIGSERIAL PRIMARY KEY,
    exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    main_seg INTEGER NOT NULL CHECK (main_seg >= 0),
    request_key TEXT NOT NULL,
    mode TEXT NOT NULL CHECK (mode IN ('auto','context','instruction')),
    input_source TEXT NOT NULL CHECK (input_source IN ('main','direct_planner')),
    raw_text TEXT NOT NULL,
    operator_text TEXT NOT NULL,
    context_snapshot JSONB NOT NULL DEFAULT '[]',
    activity_id BIGINT REFERENCES activity(id) ON DELETE SET NULL,
    assistant_text TEXT NOT NULL DEFAULT '',
    route TEXT NOT NULL DEFAULT '' CHECK (route IN ('','context','action','correction','control','clarify')),
    routing_status TEXT NOT NULL DEFAULT 'pending' CHECK (routing_status IN ('pending','running','routed','failed')),
    routing_token TEXT NOT NULL DEFAULT '',
    routing_until TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    retry_after TIMESTAMPTZ NOT NULL DEFAULT now(),
    summary TEXT NOT NULL DEFAULT '',
    clarification TEXT NOT NULL DEFAULT '',
    decision_data JSONB NOT NULL DEFAULT '{}',
    delivery_status TEXT NOT NULL DEFAULT 'none' CHECK (delivery_status IN ('none','queued','inflight','presented','needs_review')),
    delivery_owner TEXT NOT NULL DEFAULT '' CHECK (delivery_owner IN ('','planner','main')),
    delivery_token TEXT NOT NULL DEFAULT '',
    delivery_until TIMESTAMPTZ,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (exploration_id, request_key),
    CHECK (route NOT IN ('context','clarify') OR (delivery_status='none' AND delivery_owner='')),
    CHECK (delivery_status='none' OR (routing_status='routed' AND route IN ('action','correction','control') AND delivery_owner<>''))
);
CREATE INDEX idx_main_message_conversation ON main_message_routes(exploration_id, main_seg, id);
CREATE INDEX idx_main_message_routing ON main_message_routes(exploration_id, retry_after, id)
    WHERE routing_status IN ('pending','failed');
CREATE INDEX idx_main_message_delivery ON main_message_routes(exploration_id, delivery_owner, id)
    WHERE delivery_status IN ('queued','inflight');
