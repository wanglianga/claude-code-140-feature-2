-- 认购权益随房转让增强：三种处理方式、买方接受、物业材料审核、发票、提现、交割附件
-- 金额单位: 元; 比例单位: % (0~100)

-- ========== 转让单扩展 ==========
ALTER TABLE transfers
    ADD COLUMN deal_type        TEXT NOT NULL DEFAULT 'follow_house'
        CHECK (deal_type IN ('follow_house','early_exit','transfer_neighbor')),
    ADD COLUMN buyer_accepted   BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN buyer_accepted_at TIMESTAMPTZ,
    ADD COLUMN documents        TEXT NOT NULL DEFAULT '',    -- 过户材料清单（逗号分隔）
    ADD COLUMN documents_note   TEXT NOT NULL DEFAULT '',    -- 材料备注
    ADD COLUMN review_memo      TEXT NOT NULL DEFAULT '',    -- 物业审核意见
    ADD COLUMN reviewer_id      BIGINT REFERENCES users(id),
    ADD COLUMN effective_month  TEXT;                        -- 权益生效月（次月起按新权益人结算）

-- 历史数据口径兼容：旧字段 rights_transfer/refund_requested 仍保留，列表与确认逻辑以 deal_type 为准。

-- ========== 发票/票据 ==========
CREATE TABLE invoices (
    id            BIGSERIAL PRIMARY KEY,
    building_id   BIGINT NOT NULL REFERENCES buildings(id) ON DELETE CASCADE,
    invoice_no    TEXT NOT NULL UNIQUE,       -- 平台票据编号
    invoice_type  TEXT NOT NULL CHECK (invoice_type IN ('normal','red')), -- 蓝字 / 红字冲销
    parent_id     BIGINT REFERENCES invoices(id),  -- 红字票关联原蓝字票
    buyer_user_id BIGINT REFERENCES users(id),     -- 抬头/购方（个人住户）
    buyer_name    TEXT NOT NULL DEFAULT '',
    item_name     TEXT NOT NULL,                -- 项目内容
    amount        NUMERIC(12,2) NOT NULL CHECK (amount >= 0),
    tax_amount    NUMERIC(12,2) NOT NULL DEFAULT 0,
    status        TEXT NOT NULL DEFAULT 'issued' CHECK (status IN ('issued','void')),
    ref_type      TEXT NOT NULL DEFAULT '',     -- subscription/settlement/refund/transfer
    ref_id        BIGINT,
    period_month  TEXT,
    issued_by     BIGINT REFERENCES users(id),
    memo          TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_invoices_building ON invoices(building_id, created_at);
CREATE INDEX idx_invoices_user ON invoices(buyer_user_id);

-- 票据编号序列（按楼栋/年份简单递增由应用层拼号，序列保证不重号）
CREATE SEQUENCE IF NOT EXISTS invoice_seq START 1001;

-- ========== 收益提现 ==========
CREATE TABLE withdrawals (
    id           BIGSERIAL PRIMARY KEY,
    building_id  BIGINT NOT NULL REFERENCES buildings(id) ON DELETE CASCADE,
    user_id      BIGINT NOT NULL REFERENCES users(id),
    amount       NUMERIC(12,2) NOT NULL CHECK (amount > 0),
    status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','paid','rejected')),
    bank_info    TEXT NOT NULL DEFAULT '',     -- 收款账户信息（脱敏由展示层处理）
    reject_memo  TEXT NOT NULL DEFAULT '',
    reviewer_id  BIGINT REFERENCES users(id),
    paid_at      TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_withdrawals_user ON withdrawals(user_id);
