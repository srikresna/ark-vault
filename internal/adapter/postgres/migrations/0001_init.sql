-- Initial schema: replaces BadgerDB gamification state and Notion journals.
--
-- Identity: members.id is a surrogate uuid; telegram_id stays the business key
-- the bot already speaks, discord_id is an additional login identity linked to
-- the same member. Every member still originates from Telegram, so telegram_id
-- is NOT NULL and unique.
--
-- Trades are rows now, not Notion pages. deleted_at replaces archiving a page so
-- history stays auditable instead of disappearing.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE members (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    telegram_id bigint NOT NULL UNIQUE,
    discord_id  text UNIQUE,
    username    text NOT NULL DEFAULT '',
    first_name  text NOT NULL DEFAULT '',
    role        text NOT NULL DEFAULT 'member'
                CHECK (role IN ('member', 'admin', 'owner')),
    joined_at   timestamptz NOT NULL DEFAULT now(),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE trades (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    member_id        uuid NOT NULL REFERENCES members (id) ON DELETE CASCADE,
    symbol           text NOT NULL,
    direction        text NOT NULL CHECK (direction IN ('BUY', 'SELL')),
    asset_type       text NOT NULL DEFAULT '',
    status           text NOT NULL CHECK (status IN ('OPEN', 'WIN', 'LOSS', 'BE')),
    trade_date       date NOT NULL,
    result_rr        numeric(12, 4) NOT NULL DEFAULT 0,
    time_window      text NOT NULL DEFAULT '',
    confluence       text NOT NULL DEFAULT '',
    notes            text NOT NULL DEFAULT '',
    screenshot_key   text NOT NULL DEFAULT '',
    -- Telegram update that produced this row. NULL for trades imported from
    -- Notion or recorded before the ledger existed.
    source_update_id bigint,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    deleted_at       timestamptz
);

CREATE UNIQUE INDEX trades_source_update_uniq
    ON trades (source_update_id)
    WHERE source_update_id IS NOT NULL;
CREATE INDEX trades_member_date_idx ON trades (member_id, trade_date DESC);
CREATE INDEX trades_member_status_idx ON trades (member_id, status);
CREATE INDEX trades_symbol_idx ON trades (member_id, symbol);

CREATE TABLE profiles (
    member_id  uuid PRIMARY KEY REFERENCES members (id) ON DELETE CASCADE,
    total_xp   integer NOT NULL DEFAULT 0,
    level      integer NOT NULL DEFAULT 1,
    title      text NOT NULL DEFAULT 'Retail',
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE streaks (
    member_id     uuid PRIMARY KEY REFERENCES members (id) ON DELETE CASCADE,
    current_streak integer NOT NULL DEFAULT 0,
    longest_streak integer NOT NULL DEFAULT 0,
    last_log_date date,
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE xp_events (
    id         bigserial PRIMARY KEY,
    member_id  uuid NOT NULL REFERENCES members (id) ON DELETE CASCADE,
    amount     integer NOT NULL,
    reason     text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX xp_events_member_time_idx ON xp_events (member_id, created_at DESC);

-- Authoritative record of what a trade actually earned. Absence means the
-- contribution is unknown (pre-grant data); zero means it earned nothing under
-- the daily cap. Never assume a nominal base for a missing row.
CREATE TABLE trade_xp_grants (
    trade_id   uuid PRIMARY KEY REFERENCES trades (id) ON DELETE CASCADE,
    member_id  uuid NOT NULL REFERENCES members (id) ON DELETE CASCADE,
    base       integer NOT NULL DEFAULT 0,
    screenshot integer NOT NULL DEFAULT 0,
    granted_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE badges (
    member_id  uuid NOT NULL REFERENCES members (id) ON DELETE CASCADE,
    badge_id   text NOT NULL,
    awarded_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (member_id, badge_id)
);

CREATE TABLE challenges (
    year_week   text PRIMARY KEY,
    kind        text NOT NULL,
    title       text NOT NULL DEFAULT '',
    description text NOT NULL DEFAULT '',
    start_date  date NOT NULL,
    end_date    date NOT NULL,
    finalized   boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE goals (
    member_id    uuid NOT NULL REFERENCES members (id) ON DELETE CASCADE,
    year_month   text NOT NULL,
    goal_type    text NOT NULL,
    target_value numeric(12, 4) NOT NULL,
    achieved     boolean NOT NULL DEFAULT false,
    achieved_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (member_id, year_month)
);

CREATE TABLE reminder_prefs (
    member_id    uuid PRIMARY KEY REFERENCES members (id) ON DELETE CASCADE,
    enabled      boolean NOT NULL DEFAULT false,
    hour         integer NOT NULL DEFAULT 0 CHECK (hour BETWEEN 0 AND 23),
    chat_id      bigint NOT NULL DEFAULT 0,
    thread_id    integer NOT NULL DEFAULT 0,
    is_community boolean NOT NULL DEFAULT false,
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE analytics_cache (
    member_id   uuid PRIMARY KEY REFERENCES members (id) ON DELETE CASCADE,
    payload     jsonb NOT NULL,
    source_hash text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL
);

CREATE TABLE report_cards (
    member_id   uuid NOT NULL REFERENCES members (id) ON DELETE CASCADE,
    year_month  text NOT NULL,
    payload     jsonb NOT NULL,
    source_hash text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (member_id, year_month)
);

-- Replaces the Badger gam:idem: keyspace. Generic key/value because the durable
-- Telegram queue owns its own JSON shape and TTL semantics.
CREATE TABLE replay_kv (
    key        text PRIMARY KEY,
    value      bytea NOT NULL,
    expires_at timestamptz
);

CREATE INDEX replay_kv_expiry_idx ON replay_kv (expires_at)
    WHERE expires_at IS NOT NULL;

CREATE TABLE oauth_states (
    state        text PRIMARY KEY,
    provider     text NOT NULL CHECK (provider IN ('discord', 'telegram')),
    code_verifier text NOT NULL DEFAULT '',
    redirect_to  text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL
);

CREATE TABLE sessions (
    id         text PRIMARY KEY,
    member_id  uuid NOT NULL REFERENCES members (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);

CREATE INDEX sessions_member_idx ON sessions (member_id);

CREATE TABLE audit_log (
    id         bigserial PRIMARY KEY,
    member_id  uuid REFERENCES members (id) ON DELETE SET NULL,
    action     text NOT NULL,
    detail     jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_time_idx ON audit_log (created_at DESC);
