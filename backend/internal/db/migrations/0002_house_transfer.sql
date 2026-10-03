-- 认购权益随房转让（feature-2）：转让方式、买方接受、过户材料、争议记录、提现与发票
-- 金额单位: 元

-- 1) 转让申请扩展：三种处理方式 + 买方接受 + 过户材料 + 争议/审核记录
ALTER TABLE transfers ADD COLUMN mode TEXT NOT NULL DEFAULT 'with_house'
    CHECK (mode IN ('with_house','early_settlement','to_resident'));
ALTER TABLE transfers ADD COLUMN buyer_accepted    BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE transfers ADD COLUMN buyer_accepted_at TIMESTAMPTZ;
ALTER TABLE transfers ADD COLUMN materials         TEXT NOT NULL DEFAULT '[]'; -- JSON 数组：过户材料清单
ALTER TABLE transfers ADD COLUMN dispute_note      TEXT NOT NULL DEFAULT '';   -- 历史收益争议说明
ALTER TABLE transfers ADD COLUMN review_note       TEXT NOT NULL DEFAULT '';   -- 物业审核意见

-- 存量数据回填：权益不转移+退购 => 提前结算退出
UPDATE transfers SET mode='early_settlement' WHERE NOT rights_transfer AND refund_requested;
UPDATE transfers SET buyer_accepted=TRUE, buyer_accepted_at=confirmed_at
    WHERE status='confirmed' AND mode IN ('with_house','to_resident');

-- 状态机扩展：cancelled（买方拒绝/卖方撤销）
ALTER TABLE transfers DROP CONSTRAINT transfers_status_check;
ALTER TABLE transfers ADD CONSTRAINT transfers_status_check
    CHECK (status IN ('pending','confirmed','rejected','cancelled'));

-- 2) 住户提现记录（收益账户 -> 现金）
CREATE TABLE withdrawals (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users(id),
    amount     NUMERIC(12,2) NOT NULL CHECK (amount > 0),
    memo       TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_withdrawals_user ON withdrawals(user_id);

-- 3) 发票台账：认购发票 / 换开发票 / 退款（红冲）凭证
CREATE TABLE invoices (
    id              BIGSERIAL PRIMARY KEY,
    invoice_no      TEXT NOT NULL UNIQUE,
    building_id     BIGINT NOT NULL REFERENCES buildings(id) ON DELETE CASCADE,
    subscription_id BIGINT REFERENCES subscriptions(id),
    transfer_id     BIGINT REFERENCES transfers(id),
    user_id         BIGINT REFERENCES users(id),        -- 当前抬头人（可空=未注册抬头）
    holder_name     TEXT NOT NULL,                      -- 发票抬头
    inv_type        TEXT NOT NULL CHECK (inv_type IN ('subscription','reissue','refund')),
    amount          NUMERIC(12,2) NOT NULL,
    status          TEXT NOT NULL DEFAULT 'valid' CHECK (status IN ('valid','voided','replaced')),
    memo            TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_invoices_building ON invoices(building_id);
CREATE INDEX idx_invoices_sub ON invoices(subscription_id);

-- 存量已缴费认购补开认购发票（INV-S{认购ID} 号段，避免与运行时 INV000xxx 号段冲突）
INSERT INTO invoices(invoice_no, building_id, subscription_id, user_id, holder_name, inv_type, amount, memo)
SELECT 'INV-S' || s.id, s.building_id, s.id, s.user_id, u.name, 'subscription', s.amount, '存量认购补开发票'
FROM subscriptions s JOIN users u ON u.id = s.user_id
WHERE s.sub_type='shareholder' AND s.status IN ('paid','transferred') AND s.amount > 0;
