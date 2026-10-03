package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

// getRaw 原始 GET（用于 CSV 等非 JSON 响应）
func (c *client) getRaw(path, token string) (int, []byte, string) {
	req, _ := http.NewRequest("GET", c.srv.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("raw get %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, resp.Header.Get("Content-Type")
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

	// ---- 收益账户提现：甲提现 20，余额 64 → 44 ----
	st, out = c.do("POST", "/api/me/withdrawals", tA, map[string]any{"amount": 20, "memo": "测试提现"})
	if st != 201 || out["balance"].(string) != "44.00" {
		t.Fatalf("提现: %d %v", st, out)
	}
	st, out = c.do("GET", "/api/me/withdrawals", tA, nil)
	if st != 200 || len(out["items"].([]any)) != 1 {
		t.Fatalf("提现记录: %d %v", st, out)
	}
	// 超额提现应被拒
	if st, _ = c.do("POST", "/api/me/withdrawals", tA, map[string]any{"amount": 99999}); st != 409 {
		t.Fatalf("余额不足提现应 409, got %d", st)
	}

	// ---- 转让前检查：份额/未结收益/维修分摊/三种方式影响 ----
	var subA int64
	st, out = c.do("GET", fmt.Sprintf("/api/buildings/%d/subscriptions", bid), admin, nil)
	for _, it := range out["items"].([]any) {
		m := it.(map[string]any)
		if int64(m["user_id"].(float64)) == idA {
			subA = int64(m["id"].(float64))
		}
	}
	st, out = c.do("GET", fmt.Sprintf("/api/subscriptions/%d/transfer-check", subA), tA, nil)
	if st != 200 {
		t.Fatalf("transfer-check: %d %v", st, out)
	}
	if out["can_transfer"].(bool) != true || out["history_settled"].(bool) != true {
		t.Fatalf("9月已定稿，应可转让: %v", out)
	}
	if out["unsettled"].(map[string]any)["draft_count"].(float64) != 0 {
		t.Fatalf("未结收益期数应 0: %v", out["unsettled"])
	}
	if out["maintenance"].(map[string]any)["open_count"].(float64) != 0 {
		t.Fatalf("工单已解决，维修分摊敞口应 0: %v", out["maintenance"])
	}
	modes := out["modes"].(map[string]any)
	if modes["early_settlement"].(map[string]any)["ratio_after"].(float64) != 30 {
		t.Fatalf("甲退出后楼栋认购比例应 30: %v", modes)
	}
	if modes["with_house"].(map[string]any)["ratio_after"].(float64) != 70 {
		t.Fatalf("随房转让比例应不变 70: %v", modes)
	}
	// 他人不可查看该户的转让前检查（含余额）
	if st, _ = c.do("GET", fmt.Sprintf("/api/subscriptions/%d/transfer-check", subA), tB, nil); st != 403 {
		t.Fatalf("他户查看转让前检查应 403, got %d", st)
	}

	// ---- 房屋转让（随房转让）：甲 -> 丙，买方接受后物业审核 ----
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/transfers", bid), tA, map[string]any{
		"subscription_id": subA, "mode": "with_house", "to_user_id": idC,
		"materials": []string{"房屋买卖合同", "不动产权证复印件", "光伏认购权益受让确认书"},
	})
	if st != 201 {
		t.Fatalf("transfer create: %d %v", st, out)
	}
	trID := int64(out["id"].(float64))
	// 买方未接受前确认应被拒
	if st, _ = c.do("POST", fmt.Sprintf("/api/transfers/%d/confirm", trID), admin, nil); st != 409 {
		t.Fatalf("买方未接受时确认应 409, got %d", st)
	}
	// 非买方接受应 403
	if st, _ = c.do("POST", fmt.Sprintf("/api/transfers/%d/accept", trID), tA,
		map[string]any{"accept": true}); st != 403 {
		t.Fatalf("非买方接受应 403, got %d", st)
	}
	// 买方丙在线接受
	st, out = c.do("POST", fmt.Sprintf("/api/transfers/%d/accept", trID), tC, map[string]any{"accept": true})
	if st != 200 {
		t.Fatalf("买方接受: %d %v", st, out)
	}
	// 买方对历史收益提出争议（不影响办理，但会进入交割附件）
	st, out = c.do("POST", fmt.Sprintf("/api/transfers/%d/dispute", trID), tC,
		map[string]string{"note": "对 9 月维修分摊金额有异议，要求留存记录"})
	if st != 200 {
		t.Fatalf("登记争议: %d %v", st, out)
	}
	st, out = c.do("POST", fmt.Sprintf("/api/transfers/%d/confirm", trID), admin,
		map[string]string{"review_note": "过户材料齐全，审核通过"})
	if st != 200 || out["mode"] != "with_house" || out["invoice_no"] == "" {
		t.Fatalf("transfer confirm: %d %v", st, out)
	}
	// 楼栋认购比例不变（70%）
	st, out = c.do("GET", fmt.Sprintf("/api/buildings/%d", bid), admin, nil)
	if out["subscribed_ratio"].(float64) != 70 {
		t.Fatalf("随房转让后认购比例应不变: %v", out["subscribed_ratio"])
	}
	// 发票：原认购发票已换开作废，新发票抬头为丙
	st, out = c.do("GET", fmt.Sprintf("/api/buildings/%d/invoices", bid), admin, nil)
	var oldInvReplaced, reissueOK bool
	for _, it := range out["items"].([]any) {
		m := it.(map[string]any)
		if int64(m["subscription_id"].(float64)) == subA && m["inv_type"] == "subscription" && m["status"] == "replaced" {
			oldInvReplaced = true
		}
		if m["inv_type"] == "reissue" && int64(m["user_id"].(float64)) == idC &&
			m["status"] == "valid" && m["amount"].(float64) == 36000 {
			reissueOK = true
		}
	}
	if !oldInvReplaced || !reissueOK {
		t.Fatalf("随房转让发票换开不正确: replaced=%v reissue=%v", oldInvReplaced, reissueOK)
	}

	// ---- 交割附件导出（JSON + CSV） ----
	st, out = c.do("GET", fmt.Sprintf("/api/transfers/%d/export", trID), tA, nil)
	if st != 200 {
		t.Fatalf("export: %d %v", st, out)
	}
	if len(out["generation"].([]any)) != 2 || len(out["settlements"].([]any)) != 1 {
		t.Fatalf("导出应含 9 月 2 条发电与 1 条结算: gen=%v set=%v",
			len(out["generation"].([]any)), len(out["settlements"].([]any)))
	}
	if len(out["withdrawals"].([]any)) != 1 || len(out["invoices"].([]any)) != 2 {
		t.Fatalf("导出应含 1 条提现与 2 张发票: %v %v", out["withdrawals"], out["invoices"])
	}
	if out["dispute_note"].(string) == "" {
		t.Fatalf("导出应含争议记录")
	}
	// 无关住户不可导出
	if st, _ = c.do("GET", fmt.Sprintf("/api/transfers/%d/export", trID), tB, nil); st != 403 {
		t.Fatalf("无关住户导出应 403, got %d", st)
	}
	csvStatus, csvBody, csvCT := c.getRaw(fmt.Sprintf("/api/transfers/%d/export?format=csv", trID), tA)
	if csvStatus != 200 || csvCT == "" || len(csvBody) < 100 ||
		!bytes.Contains(csvBody, []byte("月度发电")) || !bytes.Contains(csvBody, []byte("已提现")) {
		t.Fatalf("CSV 导出异常: %d %s %d", csvStatus, csvCT, len(csvBody))
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

	// ---- 提前结算退出：乙出售房屋选择退出，认购款退还至收益账户 ----
	var subB int64
	st, out = c.do("GET", fmt.Sprintf("/api/buildings/%d/subscriptions", bid), admin, nil)
	for _, it := range out["items"].([]any) {
		m := it.(map[string]any)
		if int64(m["user_id"].(float64)) == idB && m["status"] == "paid" {
			subB = int64(m["id"].(float64))
		}
	}
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/transfers", bid), tB, map[string]any{
		"subscription_id": subB, "mode": "early_settlement",
		"materials": []string{"房屋买卖合同", "不动产权证复印件"},
	})
	if st != 201 {
		t.Fatalf("exit create: %d %v", st, out)
	}
	exitID := int64(out["id"].(float64))
	st, out = c.do("POST", fmt.Sprintf("/api/transfers/%d/confirm", exitID), admin,
		map[string]string{"review_note": "历史收益已结清，同意退出"})
	if st != 200 || out["mode"] != "early_settlement" {
		t.Fatalf("exit confirm: %d %v", st, out)
	}
	// 乙收益账户 = 9月 48 + 10月 144 + 退款 27000 = 27192
	st, out = c.do("GET", "/api/me", tB, nil)
	if out["balance"].(string) != "27192.00" {
		t.Fatalf("乙退出后余额=%v want 27192.00", out["balance"])
	}
	// 楼栋认购比例 70 → 40
	st, out = c.do("GET", fmt.Sprintf("/api/buildings/%d", bid), admin, nil)
	if out["subscribed_ratio"].(float64) != 40 {
		t.Fatalf("乙退出后认购比例应 40: %v", out["subscribed_ratio"])
	}
	// 发票：乙的认购发票已红冲，退款凭证 27000 已开具
	st, out = c.do("GET", fmt.Sprintf("/api/buildings/%d/invoices", bid), admin, nil)
	var voidedOK, refundInvOK bool
	for _, it := range out["items"].([]any) {
		m := it.(map[string]any)
		if int64(m["subscription_id"].(float64)) == subB && m["inv_type"] == "subscription" && m["status"] == "voided" {
			voidedOK = true
		}
		if m["inv_type"] == "refund" && int64(m["user_id"].(float64)) == idB && m["amount"].(float64) == 27000 {
			refundInvOK = true
		}
	}
	if !voidedOK || !refundInvOK {
		t.Fatalf("提前结算退出发票不正确: voided=%v refund=%v", voidedOK, refundInvOK)
	}

	// ---- 转给同楼栋住户：丙 -> 甲（非本楼住户应被拒） ----
	registerResident(c, admin, "丁(外楼)", "13100000004", "9-101")
	idD := userID(c, admin, "13100000004")
	var subC int64
	st, out = c.do("GET", fmt.Sprintf("/api/buildings/%d/subscriptions", bid), admin, nil)
	for _, it := range out["items"].([]any) {
		m := it.(map[string]any)
		if int64(m["user_id"].(float64)) == idC && m["status"] == "paid" {
			subC = int64(m["id"].(float64))
		}
	}
	st, _ = c.do("POST", fmt.Sprintf("/api/buildings/%d/transfers", bid), tC, map[string]any{
		"subscription_id": subC, "mode": "to_resident", "to_user_id": idD,
	})
	if st != 409 {
		t.Fatalf("受让人非本楼住户应 409, got %d", st)
	}
	st, out = c.do("POST", fmt.Sprintf("/api/buildings/%d/transfers", bid), tC, map[string]any{
		"subscription_id": subC, "mode": "to_resident", "to_user_id": idA,
		"materials": []string{"楼内转让协议"},
	})
	if st != 201 {
		t.Fatalf("to_resident create: %d %v", st, out)
	}
	tr2ID := int64(out["id"].(float64))
	st, out = c.do("POST", fmt.Sprintf("/api/transfers/%d/accept", tr2ID), tA, map[string]any{"accept": true})
	if st != 200 {
		t.Fatalf("甲接受: %d %v", st, out)
	}
	st, out = c.do("POST", fmt.Sprintf("/api/transfers/%d/confirm", tr2ID), admin, nil)
	if st != 200 {
		t.Fatalf("to_resident confirm: %d %v", st, out)
	}
	st, out = c.do("GET", fmt.Sprintf("/api/buildings/%d", bid), admin, nil)
	if out["subscribed_ratio"].(float64) != 40 {
		t.Fatalf("楼内转让比例应不变 40: %v", out["subscribed_ratio"])
	}

	// ---- 11 月结算：40% 份额归甲（审核通过后下一期按新权益人发放），乙/丙不再参与 ----
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
	nov := map[int64]float64{}
	for _, it := range out["items"].([]any) {
		m := it.(map[string]any)
		if m["item_type"] == "shareholder" {
			nov[int64(m["user_id"].(float64))] = m["amount"].(float64)
		}
	}
	if len(nov) != 1 || nov[idA] != 160 {
		t.Fatalf("11月仅甲按 40%% 分成 160: %v", nov)
	}
	if st, out = c.do("POST", fmt.Sprintf("/api/settlements/%d/finalize", novID), admin, nil); st != 200 {
		t.Fatalf("nov finalize: %d %v", st, out)
	}
	// 甲余额 = 64 - 20(提现) + 160 = 204
	st, out = c.do("GET", "/api/me", tA, nil)
	if out["balance"].(string) != "204.00" {
		t.Fatalf("甲余额=%v want 204.00", out["balance"])
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
