-- Таблица пользователей с коммерческой подпиской
CREATE TABLE IF NOT EXISTS users (
    id BIGSERIAL PRIMARY KEY,
    subscription_expires_at TIMESTAMPTZ NOT NULL
);

-- Таблица рекламных кампаний АРК
CREATE TABLE IF NOT EXISTS campaigns (
    id BIGINT PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    wb_token TEXT NOT NULL,
    keyword TEXT DEFAULT '',
    strategy_type VARCHAR(50) DEFAULT 'target_place',
    target_place INT DEFAULT 1,
    max_bid INT DEFAULT 1000,
    min_bid INT DEFAULT 150,
    current_bid INT DEFAULT 150,
    is_active BOOLEAN DEFAULT TRUE,
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- Таблица логов управления ставками
CREATE TABLE IF NOT EXISTS bid_logs (
    id BIGSERIAL PRIMARY KEY,
    campaign_id BIGINT NOT NULL,
    old_bid INT NOT NULL,
    new_bid INT NOT NULL,
    market_bids INT[] NOT NULL,
    status_message TEXT,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- Индексы для обеспечения высокой скорости работы на боевом сервере
CREATE INDEX IF NOT EXISTS idx_campaigns_is_active ON campaigns(is_active) WHERE is_active = TRUE;
CREATE INDEX IF NOT EXISTS idx_users_sub_expiry ON users(subscription_expires_at);
CREATE INDEX IF NOT EXISTS idx_bid_logs_campaign_id ON bid_logs(campaign_id, created_at DESC);

ALTER TABLE users ADD COLUMN allowed_auth_protocol VARCHAR(20) DEFAULT 'manual';
-- Значения: 'manual' (ручные токены), 'oauth' (автоматический протокол Wildberries), 'both' (разрешено всё)
