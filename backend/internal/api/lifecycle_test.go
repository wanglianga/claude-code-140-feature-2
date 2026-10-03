package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"pvplatform/internal/api"
	"pvplatform/internal/auth"
	"pvplatform/internal/db"
)

// adminURL 指向用于创建/删除测试库的维护连接
const adminURL = "postgres://pvapp@127.0.0.1:5433/pvplatform?sslmode=disable"
const testURL = "postgres://pvapp@127.0.0.1:5433/pvplatform_itest?sslmode=disable"

type client struct {
	t      *testing.T
	srv    *httptest.Server
	tokens map[string]string
}

func (c *client) do(method, path, token string, body any) (int, map[string]any) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.srv.URL+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("request %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

func (c *client) login(phone, pwd string) string {
	st, out := c.do("POST", "/api/auth/login", "", map[string]string{"phone": phone, "password": pwd})
	if st != 200 {
		c.t.Fatalf("login %s: %d %v", phone, st, out)
	}
	return out["token"].(string)
}

func (c *client) doRaw(method, path, token string, body any) (int, string) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.srv.URL+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("request %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func setup(t *testing.T) *client {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Skipf("测试数据库不可用，跳过集成测试: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(ctx); err != nil {
		t.Skipf("测试数据库不可用，跳过集成测试: %v", err)
	}
	if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS pvplatform_itest WITH (FORCE)`); err != nil {
		t.Skipf("无法重建测试库: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE pvplatform_itest`); err != nil {
		t.Skipf("无法创建测试库: %v", err)
	}

	pool, err := pgxpool.New(ctx, testURL)
	if err != nil {
		t.Skipf("连接测试库失败: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 直接建一个管理员
	hash, _ := auth.HashPassword("admin123")
	if _, err := pool.Exec(ctx,
		`INSERT INTO users(name,phone,role,password_hash) VALUES ('管理员','13000000000','admin',$1)`, hash); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(api.NewServer(pool, "test-secret").Router())
	t.Cleanup(srv.Close)
	return &client{t: t, srv: srv, tokens: map[string]string{}}
}

func registerResident(c *client, adminToken, name, phone, unit string) {
	st, out := c.do("POST", "/api/auth/register", adminToken,
		map[string]string{"name": name, "phone": phone, "unit_no": unit, "password": "pwd123", "role": "resident"})
	if st != 201 {
		c.t.Fatalf("register %s: %d %v", phone, st, out)
	}
}

func userID(c *client, adminToken, phone string) int64 {
	st, out := c.do("GET", "/api/users?q="+phone, adminToken, nil)
	if st != 200 {
		c.t.Fatalf("users: %d", st)
	}
	for _, it := range out["items"].([]any) {
		m := it.(map[string]any)
		if m["phone"] == phone {
			return int64(m["id"].(float64))
		}
	}
	c.t.Fatalf("user %s not found", phone)
	return 0
}

func TestFullLifecycle(t *testing.T) {
	c := setup(t)
	admin := c.login("13000000000", "admin123")

	// 三户居民
	registerResident(c, admin, "甲", "13100000001", "3-601")
	registerResident(c, admin, "乙", "13100000002", "3-501")
	registerResident(c, admin, "丙(新业主)", "13100000003", "3-401")
	tA, tB := c.login("13100000001", "pwd123"), c.login("13100000002", "pwd123")
	idA, idB, idC := userID(c, admin, "13100000001"), userID(c, admin, "13100000002"), userID(c, admin, "13100000003")

	// 建档
	st, out := c.do("POST", "/api/buildings", admin, map[string]any{
		"name": "测试楼", "total_capacity_kw": 30, "panel_total": 60,
		"unit_price": 1500, "maintenance_fund_ratio": 20,
	})
	if st != 201 {
		t.Fatalf("create building: %d %v", st, out)
	}
	bid := int64(out["id"].(float64))
	for _, id := range []int64{idA, idB, idC} {
		if st, out := c.do("POST", fmt.Sprintf("/api/buildings/%d/members", bid), admin,
			map[string]any{"user_id": id}); st != 201 {
			t.Fatalf("add member %d: %d %v", id, st, out)
		}
	}

	// 进入表决 → 全员同意 → 关表通过 → 进入认购
	if st, out := c.do("POST", fmt.Sprintf("/api/buildings/%d/stage", bid), admin,
		map[string]string{"to_stage": "voting"}); st != 200 {
		t.Fatalf("to voting: %d %v", st, out)
	}
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/votes", bid), admin,
		map[string]string{"title": "立项表决"})
	if st != 201 {
		t.Fatalf("create vote: %d", st)
	}
	voteID := int64(out["id"].(float64))
	// 第一次仅甲 1 票同意（1/3 < 2/3）→ rejected
	if st, out := c.do("POST", fmt.Sprintf("/api/votes/%d/choose", voteID), tA,
		map[string]string{"choice": "agree"}); st != 200 {
		t.Fatalf("vote: %d %v", st, out)
	}
	if st, out := c.do("POST", fmt.Sprintf("/api/votes/%d/choose", voteID), tB,
		map[string]string{"choice": "disagree"}); st != 200 {
		t.Fatalf("vote: %d %v", st, out)
	}
	st, out = c.do("POST", fmt.Sprintf("/api/votes/%d/close", voteID), admin, nil)
	if st != 200 || out["result"] != "rejected" {
		t.Fatalf("第一次表决应为 rejected, got %v", out)
	}
	// 沟通后重新表决，3 户全部同意 → approved
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/votes", bid), admin,
		map[string]string{"title": "立项表决(二次)"})
	voteID = int64(out["id"].(float64))
	tC := c.login("13100000003", "pwd123")
	for _, tk := range []string{tA, tB, tC} {
		c.do("POST", fmt.Sprintf("/api/votes/%d/choose", voteID), tk, map[string]string{"choice": "agree"})
	}
	st, out = c.do("POST", fmt.Sprintf("/api/votes/%d/close", voteID), admin, nil)
	if out["result"] != "approved" {
		t.Fatalf("二次表决应 approved, got %v", out)
	}
	if st, out := c.do("POST", fmt.Sprintf("/api/buildings/%d/stage", bid), admin,
		map[string]string{"to_stage": "subscribing"}); st != 200 {
		t.Fatalf("to subscribing: %d %v", st, out)
	}

	// 认购 甲 40%、乙 30%，缴费
	for _, x := range []struct {
		tok   string
		ratio float64
	}{{tA, 40}, {tB, 30}} {
		st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/subscribe", bid), x.tok,
			map[string]any{"type": "shareholder", "ratio": x.ratio})
		if st != 201 {
			t.Fatalf("subscribe: %d %v", st, out)
		}
		sid := int64(out["id"].(float64))
		if st, out := c.do("POST", fmt.Sprintf("/api/subscriptions/%d/pay", sid), x.tok, nil); st != 200 {
			t.Fatalf("pay: %d %v", st, out)
		}
	}
	// 超额认购应被拒
	st, _ = c.do("POST", fmt.Sprintf("/api/buildings/%d/subscribe", bid), tC,
		map[string]any{"type": "shareholder", "ratio": 40})
	if st != 409 {
		t.Fatalf("超额认购应 409, got %d", st)
	}

	// 推进至运行（施工→并网验收→运行）
	for _, stage := range []string{"construction", "grid_acceptance", "operating"} {
		if st, out := c.do("POST", fmt.Sprintf("/api/buildings/%d/stage", bid), admin,
			map[string]string{"to_stage": stage}); st != 200 {
			t.Fatalf("to %s: %d %v", stage, st, out)
		}
	}

	// 设备与发电量
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/inverters", bid), admin,
		map[string]any{"name": "1#", "capacity_kw": 15})
	inv1 := int64(out["id"].(float64))
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/inverters", bid), admin,
		map[string]any{"name": "2#", "capacity_kw": 15})
	inv2 := int64(out["id"].(float64))
	for _, x := range []struct {
		inv int64
		kwh float64
	}{{inv1, 1000}, {inv2, 800}} {
		if st, out := c.do("POST", fmt.Sprintf("/api/buildings/%d/generation", bid), admin, map[string]any{
			"period_month": "2026-09", "inverter_id": x.inv, "kwh": x.kwh,
		}); st != 200 {
			t.Fatalf("generation: %d %v", st, out)
		}
	}
	// 漏水工单：物业责任 500 元，已解决
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/work-orders", bid), tA, map[string]any{
		"period_month": "2026-09", "title": "顶楼渗水", "wo_type": "leak",
		"description": "穿层处渗水", "cost": 500,
	})
	if st != 201 {
		t.Fatalf("work order: %d %v", st, out)
	}
	woID := int64(out["id"].(float64))
	if st, out := c.do("POST", fmt.Sprintf("/api/work-orders/%d", woID), admin, map[string]any{
		"status": "resolved", "responsibility": "property", "cost": 500,
	}); st != 200 {
		t.Fatalf("wo update: %d %v", st, out)
	}

	// 生成 9 月结算（草稿）→ 校验收支 → 定稿
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/settlements", bid), admin, map[string]any{
		"period_month": "2026-09", "grid_price": 0.4, "subsidy_per_kwh": 0.05,
		"om_fee": 100, "tax": 50,
	})
	if st != 200 {
		t.Fatalf("settlement create: %d %v", st, out)
	}
	spID := int64(out["id"].(float64))
	settle := out["settlement"].(map[string]any)
	if settle["work_order_cost"].(float64) != 500 {
		t.Fatalf("wo cost = %v", settle["work_order_cost"])
	}
	if settle["fund_used"].(float64) != 0 || settle["repair_deduct"].(float64) != 500 {
		t.Fatalf("基金为 0 时维修应全额分摊: %v", settle)
	}
	st, out = c.do("GET", fmt.Sprintf("/api/settlements/%d", spID), admin, nil)
	if st != 200 {
		t.Fatalf("get settlement: %d", st)
	}
	var holderA float64
	for _, it := range out["items"].([]any) {
		m := it.(map[string]any)
		if m["item_type"] == "shareholder" && int64(m["user_id"].(float64)) == idA {
			holderA = m["amount"].(float64)
		}
	}
	// 甲 40%: 288 + 36 - 40 - 20 - 200 = 64
	if holderA != 64 {
		t.Fatalf("甲结算金额=%v want 64; items=%v", holderA, out["items"])
	}
	if st, out := c.do("POST", fmt.Sprintf("/api/settlements/%d/finalize", spID), admin, nil); st != 200 {
		t.Fatalf("finalize: %d %v", st, out)
	}
	// 定稿后禁止重算/再改工单责任
	if st, _ = c.do("POST", fmt.Sprintf("/api/buildings/%d/settlements", bid), admin, map[string]any{
		"period_month": "2026-09", "grid_price": 0.5,
	}); st != 409 {
		t.Fatalf("已定稿重算应 409, got %d", st)
	}
	if st, _ = c.do("POST", fmt.Sprintf("/api/work-orders/%d", woID), admin,
		map[string]any{"responsibility": "owner"}); st != 409 {
		t.Fatalf("已定稿周期改责任应 409, got %d", st)
	}
	// 甲余额 = 64
	st, out = c.do("GET", "/api/me", tA, nil)
	if out["balance"].(string) != "64.00" {
		t.Fatalf("甲余额=%v want 64.00", out["balance"])
	}
	// 维修基金 = 公共净收益 48 × 20% = 9.60
	st, out = c.do("GET", fmt.Sprintf("/api/buildings/%d/ledger", bid), admin, nil)
	if out["maintenance_fund_balance"].(float64) != 9.6 {
		t.Fatalf("维修基金=%v want 9.6", out["maintenance_fund_balance"])
	}

	// 房屋转让：甲把认购权益随房转给丙（买方需先接受，物业再审核过户材料）
	var subA, subB int64
	st, out = c.do("GET", fmt.Sprintf("/api/buildings/%d/subscriptions", bid), admin, nil)
	for _, it := range out["items"].([]any) {
		m := it.(map[string]any)
		if int64(m["user_id"].(float64)) == idA {
			subA = int64(m["id"].(float64))
		}
		if int64(m["user_id"].(float64)) == idB {
			subB = int64(m["id"].(float64))
		}
	}
	// 平台核查：份额、未结收益、维修分摊、买方接受状态
	st, chk := c.do("GET",
		fmt.Sprintf("/api/buildings/%d/transfer-check?subscription_id=%d&to_user_id=%d", bid, subA, idC),
		admin, nil)
	if st != 200 {
		t.Fatalf("transfer-check: %d %v", st, chk)
	}
	if chk["panel_ratio"].(float64) != 40 || chk["balance"].(float64) != 64 {
		t.Fatalf("核查份额/余额不符: %v", chk)
	}
	if chk["buyer"].(map[string]any)["accepted"].(bool) {
		t.Fatalf("买方初始应为未接受: %v", chk)
	}
	if chk["effective_month"].(string) != "2026-10" {
		t.Fatalf("生效月应为 2026-10: %v", chk["effective_month"])
	}
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/transfers", bid), tA, map[string]any{
		"subscription_id": subA, "deal_type": "follow_house", "to_user_id": idC,
	})
	if st != 201 {
		t.Fatalf("transfer create: %d %v", st, out)
	}
	trID := int64(out["id"].(float64))
	// 非买方不能代为接受
	if st, _ := c.do("POST", fmt.Sprintf("/api/transfers/%d/accept", trID), tA, nil); st != 403 {
		t.Fatalf("非买方接受应 403, got %d", st)
	}
	if st, _ := c.do("POST", fmt.Sprintf("/api/transfers/%d/accept", trID), tC, nil); st != 200 {
		t.Fatalf("买方接受失败: %d", st)
	}
	// 未登记过户材料不能确认
	if st, _ := c.do("POST", fmt.Sprintf("/api/transfers/%d/confirm", trID), admin,
		map[string]string{}); st != 400 {
		t.Fatalf("缺少过户材料应 400, got %d", st)
	}
	if st, out := c.do("POST", fmt.Sprintf("/api/transfers/%d/confirm", trID), admin, map[string]string{
		"documents": "不动产权证、买卖合同、双方身份证明、权益转让协议",
	}); st != 200 {
		t.Fatalf("transfer confirm: %d %v", st, out)
	} else if out["effective_month"].(string) != "2026-10" {
		t.Fatalf("生效月= %v want 2026-10", out["effective_month"])
	}

	// 10 月发电再结算：40% 份额应归丙，甲不再出现在住户明细
	for _, x := range []struct {
		inv int64
		kwh float64
	}{{inv1, 600}, {inv2, 600}} {
		c.do("POST", fmt.Sprintf("/api/buildings/%d/generation", bid), admin, map[string]any{
			"period_month": "2026-10", "inverter_id": x.inv, "kwh": x.kwh,
		})
	}
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/settlements", bid), admin, map[string]any{
		"period_month": "2026-10", "grid_price": 0.4,
	})
	if st != 200 {
		t.Fatalf("oct settlement: %d %v", st, out)
	}
	octID := int64(out["id"].(float64))
	st, out = c.do("GET", fmt.Sprintf("/api/settlements/%d", octID), admin, nil)
	holders := map[int64]float64{}
	for _, it := range out["items"].([]any) {
		m := it.(map[string]any)
		if m["item_type"] == "shareholder" {
			holders[int64(m["user_id"].(float64))] = m["amount"].(float64)
		}
	}
	if _, ok := holders[idA]; ok {
		t.Fatalf("甲已转让，不应再有 10 月分成: %v", holders)
	}
	// 丙 40%: 1200*0.4*0.4 = 192（无补贴无运维无维修）
	if holders[idC] != 192 {
		t.Fatalf("丙 10 月分成=%v want 192", holders[idC])
	}
	c.do("POST", fmt.Sprintf("/api/settlements/%d/finalize", octID), admin, nil)

	// 公告、异议投票、异常解释
	if st, _ := c.do("POST", fmt.Sprintf("/api/buildings/%d/announcements", bid), admin,
		map[string]string{"title": "月度公示", "content": "见结算", "pinned": "x"}); st != 201 {
		// pinned 传字符串会被 bool 解析失败 → 改成 bool 再试
	}
	if st, out := c.do("POST", fmt.Sprintf("/api/buildings/%d/announcements", bid), admin,
		map[string]any{"title": "月度公示", "content": "见结算", "pinned": true}); st != 201 {
		t.Fatalf("announcement: %d %v", st, out)
	}
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/polls", bid), tB,
		map[string]string{"title": "异议：收益偏低", "content": "要求复核"})
	pollID := int64(out["id"].(float64))
	c.do("POST", fmt.Sprintf("/api/polls/%d/vote", pollID), tA, map[string]bool{"support": true})
	c.do("POST", fmt.Sprintf("/api/polls/%d/vote", pollID), tB, map[string]bool{"support": true})
	st, out = c.do("POST", fmt.Sprintf("/api/polls/%d/close", pollID), admin, nil)
	if out["result"] != "approved" {
		t.Fatalf("异议投票应成立: %v", out)
	}
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/alerts", bid), tA, map[string]any{
		"period_month": "2026-10", "inverter_id": inv2, "reason": "2# 停机",
	})
	alertID := int64(out["id"].(float64))
	if st, out := c.do("POST", fmt.Sprintf("/api/alerts/%d/explain", alertID), admin,
		map[string]string{"explanation": "电网检修限发 2 天"}); st != 200 {
		t.Fatalf("explain: %d %v", st, out)
	}

	// 年度报告
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/annual-reports", bid), admin,
		map[string]int{"year": 2026})
	if st != 200 {
		t.Fatalf("annual report: %d %v", st, out)
	}
	rid := int64(out["id"].(float64))
	summary := out["summary"].(map[string]any)
	if summary["finalized_months"].(float64) != 2 {
		t.Fatalf("年报月份数=%v", summary["finalized_months"])
	}
	if st, out := c.do("POST", fmt.Sprintf("/api/annual-reports/%d/publish", rid), admin, nil); st != 200 {
		t.Fatalf("publish: %d %v", st, out)
	}

	// 仪表盘
	st, out = c.do("GET", fmt.Sprintf("/api/buildings/%d/dashboard", bid), admin, nil)
	if st != 200 || out["total_kwh"].(float64) != 3000 {
		t.Fatalf("dashboard: %d %v", st, out)
	}

	// ===== 认购权益随房转让：提现 / 票据冲红 / 提前退出 / 同楼栋转让 / 交割导出 =====

	// 1) 丙（10 月分成 192）申请提现 100，物业放款后余额变 92
	st, wd := c.do("POST", fmt.Sprintf("/api/buildings/%d/withdrawals", bid), tC, map[string]any{
		"amount": 100, "bank_info": "招商银行 6214…0003",
	})
	if st != 201 {
		t.Fatalf("withdrawal create: %d %v", st, wd)
	}
	if wd["available"].(float64) != 192 {
		t.Fatalf("丙可提余额应为 192: %v", wd)
	}
	wdID := int64(wd["id"].(float64))
	// 审核中再次超额提现应被拒（剩余可提 92）
	if st, _ := c.do("POST", fmt.Sprintf("/api/buildings/%d/withdrawals", bid), tC,
		map[string]any{"amount": 100, "bank_info": "x"}); st != 409 {
		t.Fatalf("超额/重复提现应 409, got %d", st)
	}
	if st, _ := c.do("POST", fmt.Sprintf("/api/withdrawals/%d/review", wdID), admin,
		map[string]any{"approve": true}); st != 200 {
		t.Fatalf("放款失败: %d", st)
	}
	st, out = c.do("GET", "/api/me", tC, nil)
	if out["balance"].(string) != "92.00" {
		t.Fatalf("丙放款后余额= %v want 92.00", out["balance"])
	}

	// 2) 票据：丙应至少有月度收益票与权益转让凭证
	st, invs := c.do("GET", fmt.Sprintf("/api/buildings/%d/invoices", bid), admin, nil)
	var transferInv, subInvA int64
	for _, it := range invs["items"].([]any) {
		m := it.(map[string]any)
		if m["ref_type"] == "transfer" && int64(m["buyer_user_id"].(float64)) == idC {
			transferInv = int64(m["id"].(float64))
		}
		if m["ref_type"] == "subscription" && int64(m["buyer_user_id"].(float64)) == idA &&
			m["status"] == "issued" && m["invoice_type"] == "normal" {
			subInvA = int64(m["id"].(float64))
		}
	}
	if transferInv == 0 {
		t.Fatalf("未找到开给丙的权益转让凭证: %v", invs["items"])
	}
	// 3) 作废甲的认购票 → 自动补开红字冲销凭证
	st, out = c.do("POST", fmt.Sprintf("/api/invoices/%d/void", subInvA), admin, nil)
	if st != 200 {
		t.Fatalf("作废票据: %d %v", st, out)
	}
	redNo := out["red_invoice_no"].(string)
	st, invs = c.do("GET", fmt.Sprintf("/api/buildings/%d/invoices", bid), admin, nil)
	foundRed := false
	for _, it := range invs["items"].([]any) {
		m := it.(map[string]any)
		if m["invoice_no"] == redNo && m["invoice_type"] == "red" &&
			m["buyer_user_id"].(float64) == float64(idA) {
			foundRed = true
		}
	}
	if !foundRed {
		t.Fatalf("未找到红字冲销凭证 %s", redNo)
	}

	// 4) 交割附件 CSV：含月度发电、维修扣费、已提现、票据各段
	stCSV, rawCSV := c.doRaw("GET",
		fmt.Sprintf("/api/buildings/%d/transfer-export?subscription_id=%d", bid, subA), admin, nil)
	if stCSV != 200 || !strings.Contains(rawCSV, "维修扣费") || !strings.Contains(rawCSV, "已提现记录") ||
		!strings.Contains(rawCSV, "2026-09") || !strings.Contains(rawCSV, "票据记录") {
		t.Fatalf("交割附件内容不符: status=%d head=%.300s", stCSV, rawCSV)
	}
	// 非历任权益人（乙）不能导出
	if st, _ := c.doRaw("GET",
		fmt.Sprintf("/api/buildings/%d/transfer-export?subscription_id=%d", bid, subA), tB, nil); st != 403 {
		t.Fatalf("非权益人交割附件应 403, got %d", st)
	}

	// 5) 乙提前结算退出：退认购款 + 冲红票据 + 楼栋认购比例 70%→40%
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/transfers", bid), tB, map[string]any{
		"subscription_id": subB, "deal_type": "early_exit",
	})
	if st != 201 {
		t.Fatalf("early_exit create: %d %v", st, out)
	}
	exitID := int64(out["id"].(float64))
	if st, out := c.do("POST", fmt.Sprintf("/api/transfers/%d/confirm", exitID), admin, map[string]string{
		"documents": "买卖合同、退出结算确认书", "review_memo": "同意提前结算退出",
	}); st != 200 {
		t.Fatalf("early_exit confirm: %d %v", st, out)
	}
	st, subs2 := c.do("GET", fmt.Sprintf("/api/buildings/%d/subscriptions", bid), admin, nil)
	if subs2["subscribed_ratio"].(float64) != 40 {
		t.Fatalf("乙退出后楼栋认购比例应为 40: %v", subs2["subscribed_ratio"])
	}
	// 乙的红字票应存在
	st, invs = c.do("GET", fmt.Sprintf("/api/buildings/%d/invoices", bid), admin, nil)
	bHasRed := false
	for _, it := range invs["items"].([]any) {
		m := it.(map[string]any)
		if m["invoice_type"] == "red" && int64(m["buyer_user_id"].(float64)) == idB {
			bHasRed = true
		}
	}
	if !bHasRed {
		t.Fatalf("乙提前退出后应有红字冲销票据")
	}

	// 6) 丙把权益转给同楼栋新住户丁：非本楼栋成员应被拒（先验拒绝路径）
	registerResident(c, admin, "丁", "13100000004", "3-301")
	idD := userID(c, admin, "13100000004")
	tD := c.login("13100000004", "pwd123")
	st, _ = c.do("POST", fmt.Sprintf("/api/buildings/%d/transfers", bid), tC, map[string]any{
		"subscription_id": subA, "deal_type": "transfer_neighbor", "to_user_id": idD,
	})
	if st != 409 {
		t.Fatalf("转给非本楼栋成员应 409, got %d", st)
	}
	if st, out := c.do("POST", fmt.Sprintf("/api/buildings/%d/members", bid), admin,
		map[string]any{"user_id": idD}); st != 201 {
		t.Fatalf("add member D: %d %v", st, out)
	}
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/transfers", bid), tC, map[string]any{
		"subscription_id": subA, "deal_type": "transfer_neighbor", "to_user_id": idD,
	})
	if st != 201 {
		t.Fatalf("transfer_neighbor create: %d %v", st, out)
	}
	nbID := int64(out["id"].(float64))
	if st, _ := c.do("POST", fmt.Sprintf("/api/transfers/%d/accept", nbID), tD, nil); st != 200 {
		t.Fatalf("丁接受权益失败: %d", st)
	}
	if st, out := c.do("POST", fmt.Sprintf("/api/transfers/%d/confirm", nbID), admin, map[string]string{
		"documents": "权益转让协议、双方身份证明",
	}); st != 200 || out["effective_month"] != "2026-11" {
		t.Fatalf("同楼栋转让确认失败或生效月错误: %d %v", st, out)
	}
	// 11 月发电结算：40% 份额应归丁（1000kWh×0.4×40% = 160）
	for _, x := range []struct {
		inv int64
		kwh float64
	}{{inv1, 500}, {inv2, 500}} {
		c.do("POST", fmt.Sprintf("/api/buildings/%d/generation", bid), admin, map[string]any{
			"period_month": "2026-11", "inverter_id": x.inv, "kwh": x.kwh,
		})
	}
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/settlements", bid), admin, map[string]any{
		"period_month": "2026-11", "grid_price": 0.4,
	})
	if st != 200 {
		t.Fatalf("nov settlement: %d %v", st, out)
	}
	novID := int64(out["id"].(float64))
	st, out = c.do("GET", fmt.Sprintf("/api/settlements/%d", novID), admin, nil)
	novHolders := map[int64]float64{}
	for _, it := range out["items"].([]any) {
		m := it.(map[string]any)
		if m["item_type"] == "shareholder" {
			novHolders[int64(m["user_id"].(float64))] = m["amount"].(float64)
		}
	}
	if novHolders[idD] != 160 {
		t.Fatalf("丁应获 11 月分成 160: %v", novHolders)
	}
	if _, ok := novHolders[idC]; ok {
		t.Fatalf("丙已转出，11 月不应再有分成: %v", novHolders)
	}
	c.do("POST", fmt.Sprintf("/api/settlements/%d/finalize", novID), admin, nil)

	// 7) 存在草稿月度结算时，物业确认转让必须被阻断
	for _, x := range []struct {
		inv int64
		kwh float64
	}{{inv1, 400}, {inv2, 400}} {
		c.do("POST", fmt.Sprintf("/api/buildings/%d/generation", bid), admin, map[string]any{
			"period_month": "2026-12", "inverter_id": x.inv, "kwh": x.kwh,
		})
	}
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/settlements", bid), admin, map[string]any{
		"period_month": "2026-12", "grid_price": 0.4,
	})
	if st != 200 {
		t.Fatalf("dec draft: %d %v", st, out)
	}
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/transfers", bid), tD, map[string]any{
		"subscription_id": subA, "deal_type": "transfer_neighbor", "to_user_id": idC,
	})
	if st != 201 {
		t.Fatalf("draft-period transfer create: %d %v", st, out)
	}
	draftTr := int64(out["id"].(float64))
	if st, _ := c.do("POST", fmt.Sprintf("/api/transfers/%d/accept", draftTr), tC, nil); st != 200 {
		t.Fatalf("丙接受（回转）失败: %d", st)
	}
	if st, out := c.do("POST", fmt.Sprintf("/api/transfers/%d/confirm", draftTr), admin, map[string]string{
		"documents": "协议",
	}); st != 409 || !strings.Contains(out["error"].(string), "未定稿") {
		t.Fatalf("草稿期确认转让应 409 且提示未定稿: %d %v", st, out)
	}

	// 阶段回退必须有原因
	if st, _ = c.do("POST", fmt.Sprintf("/api/buildings/%d/stage", bid), admin,
		map[string]string{"to_stage": "construction"}); st != 409 {
		t.Fatalf("无原因回退应 409, got %d", st)
	}
	if st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/stage", bid), admin,
		map[string]string{"to_stage": "construction", "reason": "设备供应延迟，组件到货推迟"}); st != 200 {
		t.Fatalf("有原因回退应 200: %d %v", st, out)
	}
}
