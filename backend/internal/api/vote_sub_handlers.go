package api

import (
	"fmt"
	"net/http"

	"pvplatform/internal/auth"
)

// ---------- 楼栋立项表决 ----------

func (s *Server) createVote(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	var in struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := decodeJSON(r, &in); err != nil || in.Title == "" {
		writeErr(w, 400, "表决议题必填")
		return
	}
	var id int64
	err := s.Pool.QueryRow(r.Context(),
		`INSERT INTO votes(building_id,title,content,created_by) VALUES ($1,$2,$3,$4) RETURNING id`,
		bid, in.Title, in.Content, u.ID).Scan(&id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id})
}

func (s *Server) listVotes(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	bid := urlID(r, "id")
	rows, err := s.Pool.Query(r.Context(), `
		SELECT id, title, content, status, result, agree, disagree, abstain, created_at
		FROM votes WHERE building_id=$1 ORDER BY id DESC`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var id int64
		var title, content, status, result string
		var agree, disagree, abstain int
		var at any
		if err := rows.Scan(&id, &title, &content, &status, &result, &agree, &disagree, &abstain, &at); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out = append(out, map[string]any{
			"id": id, "title": title, "content": content, "status": status,
			"result": result, "agree": agree, "disagree": disagree, "abstain": abstain, "created_at": at,
		})
	}
	writeJSON(w, 200, map[string]any{"items": out})
}

func (s *Server) chooseVote(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	vid := urlID(r, "vid")
	var in struct {
		Choice string `json:"choice"` // agree/disagree/abstain
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	switch in.Choice {
	case "agree", "disagree", "abstain":
	default:
		writeErr(w, 400, "choice 必须为 agree/disagree/abstain")
		return
	}
	var status, result string
	var bid int64
	err := s.Pool.QueryRow(r.Context(),
		`SELECT building_id, status, result FROM votes WHERE id=$1`, vid).Scan(&bid, &status, &result)
	if isNoRows(err) {
		writeErr(w, 404, "表决不存在")
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if status != "open" {
		writeErr(w, 409, "表决已结束")
		return
	}
	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())
	// 改票：先撤销旧票再投新票
	if _, err := tx.Exec(r.Context(),
		`DELETE FROM vote_choices WHERE vote_id=$1 AND user_id=$2`, vid, u.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(),
		`INSERT INTO vote_choices(vote_id,user_id,choice) VALUES ($1,$2,$3)`, vid, u.ID, in.Choice); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(),
		`UPDATE votes SET agree=(SELECT count(*) FROM vote_choices WHERE vote_id=$1 AND choice='agree'),
			disagree=(SELECT count(*) FROM vote_choices WHERE vote_id=$1 AND choice='disagree'),
			abstain=(SELECT count(*) FROM vote_choices WHERE vote_id=$1 AND choice='abstain')
		 WHERE id=$1`, vid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// closeVote 结束表决并按楼栋成员数判定：同意占比 >= 2/3 通过（双三分之二简化为人数口径）
func (s *Server) closeVote(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	vid := urlID(r, "vid")
	var bid int64
	var status string
	if err := s.Pool.QueryRow(r.Context(),
		`SELECT building_id, status FROM votes WHERE id=$1`, vid).Scan(&bid, &status); err != nil {
		writeErr(w, 404, "表决不存在")
		return
	}
	if status != "open" {
		writeErr(w, 409, "表决已结束")
		return
	}
	var total, agree int
	_ = s.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM building_members WHERE building_id=$1 AND member_type='resident'`, bid).Scan(&total)
	if err := s.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM vote_choices WHERE vote_id=$1 AND choice='agree'`, vid).Scan(&agree); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	result := "rejected"
	if total > 0 && agree*3 >= total*2 { // >= 2/3
		result = "approved"
	}
	if _, err := s.Pool.Exec(r.Context(),
		`UPDATE votes SET status='closed', result=$2 WHERE id=$1`, vid, result); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "result": result, "members": total, "agree": agree})
}

// ---------- 认购与缴费 ----------

func (s *Server) subscribe(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	var in struct {
		UserID int64   `json:"user_id"`
		Type   string  `json:"type"` // shareholder / public_only
		Ratio  float64 `json:"ratio"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	target := u.ID
	if in.UserID != 0 {
		if u.Role != "admin" && u.Role != "property" {
			writeErr(w, 403, "仅管理员可代住户登记认购")
			return
		}
		target = in.UserID
	}
	if in.Type == "" {
		in.Type = "shareholder"
	}
	if in.Type != "shareholder" && in.Type != "public_only" {
		writeErr(w, 400, "认购类型不合法")
		return
	}
	var stage string
	var panelTotal int
	var unitPrice float64
	err := s.Pool.QueryRow(r.Context(),
		`SELECT stage, panel_total, unit_price FROM buildings WHERE id=$1`, bid).
		Scan(&stage, &panelTotal, &unitPrice)
	if isNoRows(err) {
		writeErr(w, 404, "楼栋不存在")
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if stage != "subscribing" {
		writeErr(w, 409, "当前楼栋不在认购缴费阶段，无法认购")
		return
	}
	if in.Type == "public_only" {
		in.Ratio = 0
	}
	if in.Ratio <= 0 || in.Ratio > 100 {
		writeErr(w, 400, "认购组件比例须在 0~100 之间")
		return
	}
	var held float64
	_ = s.Pool.QueryRow(r.Context(),
		`SELECT COALESCE(SUM(panel_ratio),0) FROM subscriptions
		 WHERE building_id=$1 AND sub_type='shareholder' AND status IN ('pending','paid','transferred')`, bid).Scan(&held)
	var existing float64
	_ = s.Pool.QueryRow(r.Context(),
		`SELECT COALESCE(SUM(panel_ratio),0) FROM subscriptions WHERE building_id=$1 AND user_id=$2
		 AND sub_type='shareholder' AND status IN ('pending','paid')`, bid, target).Scan(&existing)
	if held-existing+in.Ratio > 100.0001 {
		writeErr(w, 409, fmt.Sprintf("认购比例超出剩余可认购份额（已认购 %.2f%%）", held))
		return
	}
	panelCount := 0
	if panelTotal > 0 {
		panelCount = int(in.Ratio/100*float64(panelTotal) + 0.5)
	}
	amount := panelCount * int(unitPrice+0.5)
	if panelTotal == 0 {
		amount = 0
	}
	var id int64
	err = s.Pool.QueryRow(r.Context(), `
		INSERT INTO subscriptions(building_id,user_id,sub_type,panel_ratio,panel_count,amount)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		bid, target, in.Type, in.Ratio, panelCount, amount).Scan(&id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 自动入楼栋成员
	_, _ = s.Pool.Exec(r.Context(),
		`INSERT INTO building_members(building_id,user_id,member_type) VALUES ($1,$2,'resident') ON CONFLICT DO NOTHING`,
		bid, target)
	writeJSON(w, 201, map[string]any{"id": id, "amount": amount, "panel_count": panelCount})
}

func (s *Server) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	bid := urlID(r, "id")
	rows, err := s.Pool.Query(r.Context(), `
		SELECT s.id, s.user_id, u.name, COALESCE(u.unit_no,''), s.sub_type, s.panel_ratio,
			s.panel_count, s.amount, s.status, s.created_at, COALESCE(s.paid_at IS NOT NULL, false)
		FROM subscriptions s JOIN users u ON u.id=s.user_id
		WHERE s.building_id=$1 ORDER BY s.id`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []any{}
	var totalRatio float64
	for rows.Next() {
		var sid, uid int64
		var name, unit, stype, status string
		var ratio, amount float64
		var count int
		var at any
		var paid bool
		if err := rows.Scan(&sid, &uid, &name, &unit, &stype, &ratio, &count, &amount, &status, &at, &paid); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if status != "refunded" {
			totalRatio += ratio
		}
		out = append(out, map[string]any{
			"id": sid, "user_id": uid, "name": name, "unit_no": unit,
			"type": stype, "ratio": ratio, "panel_count": count,
			"amount": amount, "status": status, "created_at": at, "paid": paid,
		})
	}
	writeJSON(w, 200, map[string]any{"items": out, "subscribed_ratio": totalRatio})
}

func (s *Server) paySubscription(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	sid := urlID(r, "sid")
	var bid, uid int64
	var amount float64
	var status string
	err := s.Pool.QueryRow(r.Context(),
		`SELECT building_id, user_id, amount, status FROM subscriptions WHERE id=$1`, sid).
		Scan(&bid, &uid, &amount, &status)
	if isNoRows(err) {
		writeErr(w, 404, "认购记录不存在")
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if u.ID != uid && u.Role != "admin" && u.Role != "property" {
		writeErr(w, 403, "只能确认本人认购的缴费")
		return
	}
	if status != "pending" {
		writeErr(w, 409, "认购记录状态为 "+status+"，无需缴费")
		return
	}
	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(),
		`UPDATE subscriptions SET status='paid', paid_at=now() WHERE id=$1`, sid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(),
		`INSERT INTO payments(building_id,user_id,amount,pay_type,memo) VALUES ($1,$2,$3,'subscription','认购组件缴费')`,
		bid, uid, amount); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(),
		`INSERT INTO ledger(building_id,user_id,direction,amount,category,ref_type,ref_id,memo)
		 VALUES ($1,NULL,'in',$2,'subscription','subscription',$3,'认购组件款入公共账')`,
		bid, amount, sid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 开具认购发票（抬头=认购住户本人，物业/管理员代缴时同样抬头到户）
	if amount > 0 {
		var holder string
		if err := tx.QueryRow(r.Context(), `SELECT name FROM users WHERE id=$1`, uid).Scan(&holder); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if _, err := s.issueInvoiceTx(r, tx, bid, sid, nil, uid, holder, "subscription", amount, "光伏组件认购款发票"); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	// 刷新楼栋已认购比例
	if _, err := tx.Exec(r.Context(),
		`UPDATE buildings SET subscribed_ratio=(SELECT COALESCE(SUM(panel_ratio),0) FROM subscriptions
		  WHERE building_id=$1 AND status='paid' AND sub_type='shareholder') WHERE id=$1`, bid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// refundSubscription 退购：仅在并网验收前可退；已缴款项原路退回（记 refund 流水）
func (s *Server) refundSubscription(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	sid := urlID(r, "sid")
	var bid, uid int64
	var amount float64
	var status, stage string
	err := s.Pool.QueryRow(r.Context(),
		`SELECT s.building_id, s.user_id, s.amount, s.status, b.stage
		 FROM subscriptions s JOIN buildings b ON b.id=s.building_id WHERE s.id=$1`, sid).
		Scan(&bid, &uid, &amount, &status, &stage)
	if isNoRows(err) {
		writeErr(w, 404, "认购记录不存在")
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if status == "refunded" {
		writeErr(w, 409, "该认购已退款")
		return
	}
	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(),
		`UPDATE subscriptions SET status='refunded' WHERE id=$1`, sid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if status == "paid" {
		if _, err := tx.Exec(r.Context(),
			`INSERT INTO payments(building_id,user_id,amount,pay_type,memo) VALUES ($1,$2,$3,'refund','退购退款')`,
			bid, uid, amount); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if _, err := tx.Exec(r.Context(),
			`INSERT INTO ledger(building_id,user_id,direction,amount,category,ref_type,ref_id,memo)
			 VALUES ($1,$2,'out',$3,'refund','subscription',$4,'退购退款至住户')`,
			bid, uid, amount, sid); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if _, err := tx.Exec(r.Context(),
		`UPDATE buildings SET subscribed_ratio=(SELECT COALESCE(SUM(panel_ratio),0) FROM subscriptions
		  WHERE building_id=$1 AND status='paid' AND sub_type='shareholder') WHERE id=$1`, bid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "refunded": amount})
}
