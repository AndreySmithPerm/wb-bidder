-- 1. Таблица пользователей с коммерческой подпиской
CREATE TABLE IF NOT EXISTS users (
    id BIGSERIAL PRIMARY KEY,
    email VARCHAR(255) NOT NULL UNIQUE,
    password_hash VARCHAR(255) NOT NULL,
    subscription_expires_at TIMESTAMPTZ, -- ИСПРАВЛЕНО: убран NOT NULL для корректной поддержки указателей (*time.Time) в Go
    campaign_limit INT DEFAULT 1,
    role VARCHAR(20) DEFAULT 'seller',
    allowed_auth_protocol VARCHAR(20) DEFAULT 'manual', -- Значения: 'manual', 'oauth', 'both'
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- 2. Таблица настроек системы (для извлечения триалов и цены на лендинге)
CREATE TABLE IF NOT EXISTS system_settings (
    key VARCHAR(50) PRIMARY KEY,
    value VARCHAR(255) NOT NULL
);

-- Инициализация системных настроек по умолчанию
INSERT INTO system_settings (key, value) VALUES 
('trial_days', '3'),
('trial_campaign_limit', '1'),
('subscription_price', '2990')
ON CONFLICT (key) DO NOTHING;

-- 3. Таблица рекламных кампаний АРК
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

-- 4. Таблица логов управления ставками
CREATE TABLE IF NOT EXISTS bid_logs (
    id BIGSERIAL PRIMARY KEY,
    campaign_id BIGINT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE, -- ИСПРАВЛЕНО: добавлен FOREIGN KEY с каскадным удалением логов
    old_bid INT NOT NULL,
    new_bid INT NOT NULL,
    market_bids INT[] NOT NULL, -- Полностью совместимо с типом []int32 в jackc/pgx/v5
    status_message TEXT,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- 5. Индексы для обеспечения высокой скорости работы на боевом сервере
CREATE INDEX IF NOT EXISTS idx_campaigns_is_active ON campaigns(is_active) WHERE is_active = TRUE;
CREATE INDEX IF NOT EXISTS idx_users_sub_expiry ON users(subscription_expires_at);
CREATE INDEX IF NOT EXISTS idx_users_email ON users(email);
CREATE INDEX IF NOT EXISTS idx_bid_logs_campaign_id ON bid_logs(campaign_id, created_at DESC);
