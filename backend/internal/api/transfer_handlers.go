package api

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"pvplatform/internal/auth"
)

// ---------- 认购权益随房转让 ----------
//
// 业务规则：
//  1. 住户出售房屋前，平台先做转让前检查（transfer-check）：认购份额、未结收益、
//     维修分摊敞口、收益账户余额，并给出三种处理方式对收益账户/发票/楼栋认购比例的影响。
//  2. 卖方可选：with_house 随房转让 / early_settlement 提前结算退出 / to_resident 转给同楼栋住户。
//  3. 随房转让与楼内转让须买方接受权益（在线确认或物业代录线下确认）；
//     物业审核过户材料后办理，下一期收益才按新权益人发放（存在未定稿结算期时拒绝办理）。
//  4. 提前结算退出：历史收益须已结清，认购款退还至卖方收益账户，原认购发票红冲并开具退款凭证，
//     楼栋认购比例下降；随房/楼内转让认购比例不变，认购发票换开给新权益人。
//  5. 过户期间买卖双方对历史收益有争议时，可导出交割附件（月度发电、结算、维修扣费、已提现记录）。

// transferCheck 转让前检查：份额 / 未结收益 / 维修分摊 / 余额 / 三种方式影响预览
func (s *Server) transferCheck(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	sid := urlID(r, "sid")
	out, status, msg := s.buildTransferCheck(r, sid)
	if status != 200 {
		writeErr(w, status, msg)
		return
	}
	writeJSON(w, 200, out)
}

// buildTransferCheck 计算转让前检查快照（供 GET 与发起申请时复用）
func (s *Server) buildTransferCheck(r *http.Request, sid int64) (map[string]any, int, string) {
	ctx := r.Context()
	var bid, uid int64
	var name, unit, stype, status string
	var ratio, amount float64
	var panelCount int
	err := s.Pool.QueryRow(ctx, `
		SELECT s.building_id, s.user_id, u.name, COALESCE(u.unit_no,''), s.sub_type,
			s.panel_ratio, s.panel_count, s.amount, s.status
		FROM subscriptions s JOIN users u ON u.id=s.user_id WHERE s.id=$1`, sid).
		Scan(&bid, &uid, &name, &unit, &stype, &ratio, &panelCount, &amount, &status)
	if isNoRows(err) {
		return nil, 404, "认购记录不存在"
	} else if err != nil {
		return nil, 500, err.Error()
	}
	// 检查含余额等敏感信息，仅本户或物业/管理员可见
	if cu := auth.CurrentUser(r); cu == nil ||
		(cu.ID != uid && cu.Role != "admin" && cu.Role != "property") {
		return nil, 403, "仅本户或物业可查看转让前检查"
	}

	// 收益账户余额（已结算未提现）
	var balance string
	_ = s.Pool.QueryRow(ctx, `SELECT balance FROM users WHERE id=$1`, uid).Scan(&balance)

	// 未结收益：草稿结算期中该户的明细
	type draftRow struct {
		Month  string  `json:"period_month"`
		Amount float64 `json:"amount"`
	}
	drafts := []draftRow{}
	var unsettledTotal float64
	dRows, err := s.Pool.Query(ctx, `
		SELECT sp.period_month, si.amount
		FROM settlement_items si JOIN settlement_periods sp ON sp.id=si.period_id
		WHERE sp.building_id=$1 AND sp.status='draft' AND si.user_id=$2 AND si.item_type='shareholder'
		ORDER BY sp.period_month`, bid, uid)
	if err != nil {
		return nil, 500, err.Error()
	}
	defer dRows.Close()
	for dRows.Next() {
		var d draftRow
		if err := dRows.Scan(&d.Month, &d.Amount); err != nil {
			return nil, 500, err.Error()
		}
		drafts = append(drafts, d)
		unsettledTotal += d.Amount
	}

	// 维修分摊敞口：未解决且费用会摊到住户的工单（property/owner/待判定）
	type woRow struct {
		ID             int64   `json:"id"`
		Month          string  `json:"period_month"`
		Title          string  `json:"title"`
		Cost           float64 `json:"cost"`
		Responsibility string  `json:"responsibility"`
		Status         string  `json:"status"`
	}
	openWOs := []woRow{}
	var exposure float64
	wRows, err := s.Pool.Query(ctx, `
		SELECT id, period_month, title, cost, responsibility, status FROM work_orders
		WHERE building_id=$1 AND cost>0 AND status<>'resolved'
		  AND responsibility IN ('property','owner','undetermined')
		ORDER BY id`, bid)
	if err != nil {
		return nil, 500, err.Error()
	}
	defer wRows.Close()
	for wRows.Next() {
		var wo woRow
		if err := wRows.Scan(&wo.ID, &wo.Month, &wo.Title, &wo.Cost, &wo.Responsibility, &wo.Status); err != nil {
			return nil, 500, err.Error()
		}
		openWOs = append(openWOs, wo)
		exposure += wo.Cost
	}
	// 草稿结算期中该户已计入的维修分摊
	var draftWoShare float64
	_ = s.Pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(si.wo_share),0)
		FROM settlement_items si JOIN settlement_periods sp ON sp.id=si.period_id
		WHERE sp.building_id=$1 AND sp.status='draft' AND si.user_id=$2 AND si.item_type='shareholder'`,
		bid, uid).Scan(&draftWoShare)

	var subRatio float64
	_ = s.Pool.QueryRow(ctx, `SELECT subscribed_ratio FROM buildings WHERE id=$1`, bid).Scan(&subRatio)

	ratioAfterExit := subRatio
	if stype == "shareholder" && status == "paid" {
		ratioAfterExit = subRatio - ratio
	}
	modes := map[string]any{
		"with_house": map[string]any{
			"label":          "随房转让（权益过户给买方）",
			"ratio_after":    subRatio,
			"account_effect": "卖方收益账户余额不变；历史已结算收益仍归卖方",
			"invoice_effect": "原认购发票换开给买方",
			"earnings":       "物业审核通过后，下一期收益起按新权益人（买方）发放",
		},
		"early_settlement": map[string]any{
			"label":          "提前结算退出（卖方退出认购）",
			"ratio_after":    ratioAfterExit,
			"account_effect": fmt.Sprintf("认购款 ¥%.2f 退还至卖方收益账户，可提现", amount),
			"invoice_effect": "原认购发票红冲作废，开具退款凭证",
			"earnings":       "历史收益须先全部定稿结清；退出后不再参与后续月份分成",
		},
		"to_resident": map[string]any{
			"label":          "转给同楼栋其他住户",
			"ratio_after":    subRatio,
			"account_effect": "卖方收益账户余额不变；历史已结算收益仍归卖方",
			"invoice_effect": "原认购发票换开给受让住户",
			"earnings":       "物业审核通过后，下一期收益起按新权益人（受让住户）发放",
		},
	}

	blockers := []string{}
	if status != "paid" {
		blockers = append(blockers, "认购状态为 "+status+"，仅已缴费认购可办理")
	}
	if stype != "shareholder" {
		blockers = append(blockers, "仅组件认购（shareholder）含可转让权益")
	}
	var pendingTransfer int
	_ = s.Pool.QueryRow(ctx,
		`SELECT count(*) FROM transfers WHERE subscription_id=$1 AND status='pending'`, sid).Scan(&pendingTransfer)
	if pendingTransfer > 0 {
		blockers = append(blockers, "该认购已有待审核的转让申请")
	}

	return map[string]any{
		"subscription": map[string]any{
			"id": sid, "user_id": uid, "name": name, "unit_no": unit,
			"type": stype, "ratio": ratio, "panel_count": panelCount,
			"amount": amount, "status": status,
		},
		"balance": balance,
		"unsettled": map[string]any{
			"draft_count": len(drafts), "total": unsettledTotal, "periods": drafts,
		},
		"maintenance": map[string]any{
			"open_count": len(openWOs), "exposure": exposure,
			"draft_wo_share": draftWoShare, "work_orders": openWOs,
		},
		"building":        map[string]any{"id": bid, "subscribed_ratio": subRatio},
		"modes":           modes,
		"history_settled": len(drafts) == 0,
		"can_transfer":    len(blockers) == 0,
		"blockers":        blockers,
	}, 200, ""
}

// createTransfer 卖方（或物业代办）发起转让申请
func (s *Server) createTransfer(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	var in struct {
		SubscriptionID  int64    `json:"subscription_id"`
		Mode            string   `json:"mode"` // with_house / early_settlement / to_resident
		ToUserID        int64    `json:"to_user_id"`
		ToName          string   `json:"to_name"`
		ToUnit          string   `json:"to_unit"`
		Materials       []string `json:"materials"`
		Memo            string   `json:"memo"`
		RightsTransfer  *bool    `json:"rights_transfer"`  // 兼容旧字段
		RefundRequested bool     `json:"refund_requested"` // 兼容旧字段
	}
	if err := decodeJSON(r, &in); err != nil || in.SubscriptionID == 0 {
		writeErr(w, 400, "subscription_id 必填")
		return
	}
	// 旧字段映射：rights_transfer=true → 随房转让；refund_requested → 提前结算退出
	if in.Mode == "" {
		switch {
		case in.RightsTransfer != nil && !*in.RightsTransfer && in.RefundRequested:
			in.Mode = "early_settlement"
		case in.RefundRequested:
			in.Mode = "early_settlement"
		default:
			in.Mode = "with_house"
		}
	}
	if in.Mode != "with_house" && in.Mode != "early_settlement" && in.Mode != "to_resident" {
		writeErr(w, 400, "mode 须为 with_house / early_settlement / to_resident")
		return
	}

	var subBid, fromID int64
	var status, stype string
	err := s.Pool.QueryRow(r.Context(),
		`SELECT building_id, user_id, status, sub_type FROM subscriptions WHERE id=$1`, in.SubscriptionID).
		Scan(&subBid, &fromID, &status, &stype)
	if isNoRows(err) {
		writeErr(w, 404, "认购记录不存在")
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if subBid != bid {
		writeErr(w, 400, "认购记录不属于该楼栋")
		return
	}
	if u.ID != fromID && u.Role != "admin" && u.Role != "property" {
		writeErr(w, 403, "只能为本人认购发起房屋转让")
		return
	}
	if status != "paid" || stype != "shareholder" {
		writeErr(w, 409, "仅已缴费的组件认购可以办理转让/退出，当前状态："+status)
		return
	}
	var pending int
	_ = s.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM transfers WHERE subscription_id=$1 AND status='pending'`, in.SubscriptionID).Scan(&pending)
	if pending > 0 {
		writeErr(w, 409, "该认购已有待审核的转让申请，请先处理")
		return
	}

	// 受让人校验
	if in.Mode == "to_resident" {
		if in.ToUserID == 0 {
			writeErr(w, 400, "转给同楼栋住户时须选择受让住户（to_user_id）")
			return
		}
		if in.ToUserID == fromID {
			writeErr(w, 400, "受让人不能是卖方本人")
			return
		}
		var member int
		_ = s.Pool.QueryRow(r.Context(),
			`SELECT count(*) FROM building_members WHERE building_id=$1 AND user_id=$2 AND member_type='resident'`,
			bid, in.ToUserID).Scan(&member)
		if member == 0 {
			writeErr(w, 409, "受让人不是本楼栋住户，不能按“转给同楼栋住户”办理")
			return
		}
	}
	if in.Mode == "with_house" && in.ToUserID == 0 && in.ToName == "" {
		writeErr(w, 400, "随房转让需选择买方（已注册用户）或填写买方姓名")
		return
	}
	if in.ToUserID != 0 && in.ToUserID == fromID {
		writeErr(w, 400, "受让人不能是卖方本人")
		return
	}

	materials, _ := json.Marshal(in.Materials)
	if in.Materials == nil {
		materials = []byte("[]")
	}

	var pendingDraft int
	_ = s.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM settlement_periods WHERE building_id=$1 AND status='draft'`, bid).Scan(&pendingDraft)

	// 检查快照须在插入前计算（否则会被自己的待审核申请挡住）
	check, _, _ := s.buildTransferCheck(r, in.SubscriptionID)

	var id int64
	err = s.Pool.QueryRow(r.Context(), `
		INSERT INTO transfers(building_id,subscription_id,from_user_id,to_user_id,to_name,to_unit,
			mode,rights_transfer,refund_requested,history_settled,materials,memo)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id`,
		bid, in.SubscriptionID, fromID, nullableID(in.ToUserID), in.ToName, in.ToUnit,
		in.Mode, in.Mode != "early_settlement", in.Mode == "early_settlement",
		pendingDraft == 0, string(materials), in.Memo).Scan(&id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{
		"id": id, "mode": in.Mode, "history_settled": pendingDraft == 0, "check": check,
	})
}

func (s *Server) listTransfers(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	bid := urlID(r, "id")
	rows, err := s.Pool.Query(r.Context(), `
		SELECT t.id, t.subscription_id, f.id, f.name, COALESCE(f.unit_no,''),
			COALESCE(t.to_user_id,0), COALESCE(tt.name,t.to_name), COALESCE(tt.unit_no,t.to_unit),
			t.mode, t.rights_transfer, t.refund_requested, t.history_settled, t.status,
			COALESCE(t.memo,''), t.materials, t.buyer_accepted, t.buyer_accepted_at,
			COALESCE(t.dispute_note,''), COALESCE(t.review_note,''),
			s.panel_ratio, s.amount, t.created_at, t.confirmed_at
		FROM transfers t
		JOIN users f ON f.id=t.from_user_id
		JOIN subscriptions s ON s.id=t.subscription_id
		LEFT JOIN users tt ON tt.id=t.to_user_id
		WHERE t.building_id=$1 ORDER BY t.id DESC`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var id, sid, fromID, toID int64
		var fromName, fromUnit, toName, toUnit, mode, status, memo string
		var materialsJSON, dispute, review string
		var rights, refund, settled, accepted bool
		var acceptedAt, createdAt, confirmedAt any
		var ratio, amount float64
		if err := rows.Scan(&id, &sid, &fromID, &fromName, &fromUnit, &toID, &toName, &toUnit,
			&mode, &rights, &refund, &settled, &status, &memo, &materialsJSON, &accepted, &acceptedAt,
			&dispute, &review, &ratio, &amount, &createdAt, &confirmedAt); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		var materials []string
		_ = json.Unmarshal([]byte(materialsJSON), &materials)
		if materials == nil {
			materials = []string{}
		}
		out = append(out, map[string]any{
			"id": id, "subscription_id": sid,
			"from_user_id": fromID, "from_name": fromName, "from_unit": fromUnit,
			"to_user_id": toID, "to_name": toName, "to_unit": toUnit,
			"mode": mode, "rights_transfer": rights, "refund_requested": refund,
			"history_settled": settled, "status": status, "memo": memo,
			"materials": materials, "buyer_accepted": accepted, "buyer_accepted_at": acceptedAt,
			"dispute_note": dispute, "review_note": review,
			"sub_ratio": ratio, "sub_amount": amount,
			"created_at": createdAt, "confirmed_at": confirmedAt,
		})
	}
	writeJSON(w, 200, map[string]any{"items": out})
}

// loadTransfer 读取转让申请公共字段
type transferRow struct {
	ID, Bid, Sid, FromID int64
	ToID                 *int64
	ToName, ToUnit       string
	Mode, Status         string
	BuyerAccepted        bool
}

func (s *Server) loadTransfer(r *http.Request, tid int64) (*transferRow, int, string) {
	var t transferRow
	err := s.Pool.QueryRow(r.Context(), `
		SELECT id, building_id, subscription_id, from_user_id, to_user_id,
			COALESCE(to_name,''), COALESCE(to_unit,''), mode, status, buyer_accepted
		FROM transfers WHERE id=$1`, tid).
		Scan(&t.ID, &t.Bid, &t.Sid, &t.FromID, &t.ToID, &t.ToName, &t.ToUnit, &t.Mode, &t.Status, &t.BuyerAccepted)
	if isNoRows(err) {
		return nil, 404, "转让申请不存在"
	} else if err != nil {
		return nil, 500, err.Error()
	}
	return &t, 200, ""
}

// acceptTransfer 买方接受/拒绝受让权益；物业可代录线下已签署的受让确认
func (s *Server) acceptTransfer(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	tid := urlID(r, "tid")
	var in struct {
		Accept bool   `json:"accept"`
		Note   string `json:"note"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	t, status, msg := s.loadTransfer(r, tid)
	if t == nil {
		writeErr(w, status, msg)
		return
	}
	if t.Status != "pending" {
		writeErr(w, 409, "该转让申请已处理："+t.Status)
		return
	}
	if t.Mode == "early_settlement" {
		writeErr(w, 400, "提前结算退出无需买方接受")
		return
	}
	isBuyer := t.ToID != nil && *t.ToID == u.ID
	isManager := u.Role == "admin" || u.Role == "property"
	if !isBuyer && !isManager {
		writeErr(w, 403, "仅买方本人或物业可确认接受权益")
		return
	}
	who := "买方在线确认"
	if isManager && !isBuyer {
		who = "物业代录线下确认（" + u.Name + "）"
	}
	if !in.Accept {
		if _, err := s.Pool.Exec(r.Context(),
			`UPDATE transfers SET status='cancelled',
				review_note=review_note||$2 WHERE id=$1`,
			tid, fmt.Sprintf("\n[%s] 买方拒绝接受权益。%s", who, in.Note)); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "status": "cancelled"})
		return
	}
	if _, err := s.Pool.Exec(r.Context(),
		`UPDATE transfers SET buyer_accepted=TRUE, buyer_accepted_at=now(),
			review_note=review_note||$2 WHERE id=$1`,
		tid, fmt.Sprintf("\n[%s] 买方接受光伏认购权益。%s", who, in.Note)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "buyer_accepted": true})
}

// disputeTransfer 过户期间买卖双方（或物业）登记历史收益争议
func (s *Server) disputeTransfer(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	tid := urlID(r, "tid")
	var in struct {
		Note string `json:"note"`
	}
	if err := decodeJSON(r, &in); err != nil || in.Note == "" {
		writeErr(w, 400, "争议说明（note）必填")
		return
	}
	t, status, msg := s.loadTransfer(r, tid)
	if t == nil {
		writeErr(w, status, msg)
		return
	}
	involved := u.ID == t.FromID || (t.ToID != nil && *t.ToID == u.ID) ||
		u.Role == "admin" || u.Role == "property"
	if !involved {
		writeErr(w, 403, "仅买卖双方或物业可登记争议")
		return
	}
	line := fmt.Sprintf("[%s · %s] %s", u.Name, time.Now().Format("2006-01-02 15:04"), in.Note)
	if _, err := s.Pool.Exec(r.Context(),
		`UPDATE transfers SET dispute_note = CASE WHEN dispute_note='' THEN $2 ELSE dispute_note||E'\n'||$2 END
		 WHERE id=$1`, tid, line); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// confirmTransfer 物业审核过户材料后办理：
//   - with_house / to_resident：认购过户给新权益人，历史收益仍归卖方，下一期起按新权益人发放；
//     认购发票换开；楼栋认购比例不变。
//   - early_settlement：认购退出，认购款退还至卖方收益账户；原发票红冲并开退款凭证；楼栋认购比例下降。
//   - 存在未定稿月度结算（历史收益未结清）时拒绝办理。
func (s *Server) confirmTransfer(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	tid := urlID(r, "tid")
	var in struct {
		ReviewNote string `json:"review_note"`
		ToUserID   int64  `json:"to_user_id"` // 买方后补注册时绑定账号
	}
	_ = decodeJSON(r, &in)

	t, status, msg := s.loadTransfer(r, tid)
	if t == nil {
		writeErr(w, status, msg)
		return
	}
	if t.Status != "pending" {
		writeErr(w, 409, "该转让申请已处理："+t.Status)
		return
	}
	// 后补绑定买方账号
	if in.ToUserID != 0 && t.ToID == nil {
		if _, err := s.Pool.Exec(r.Context(),
			`UPDATE transfers SET to_user_id=$2 WHERE id=$1`, tid, in.ToUserID); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		t.ToID = &in.ToUserID
	}

	var pendingDraft int
	_ = s.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM settlement_periods WHERE building_id=$1 AND status='draft'`, t.Bid).Scan(&pendingDraft)
	if pendingDraft > 0 {
		writeErr(w, 409, "存在未最终定稿的月度结算，历史收益未结清，请先生成/定稿或删除草稿后再办理")
		return
	}

	var subAmount, subRatio float64
	var subStatus string
	if err := s.Pool.QueryRow(r.Context(),
		`SELECT amount, panel_ratio, status FROM subscriptions WHERE id=$1`, t.Sid).
		Scan(&subAmount, &subRatio, &subStatus); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if subStatus != "paid" {
		writeErr(w, 409, "认购当前状态不是已缴费，无法确认办理")
		return
	}

	// 模式校验：买方接受权益 / 楼内住户资格
	if t.Mode == "with_house" || t.Mode == "to_resident" {
		if t.ToID == nil {
			writeErr(w, 409, "买方尚未注册平台账号，请先注册并在审核时绑定（to_user_id）")
			return
		}
		if !t.BuyerAccepted {
			writeErr(w, 409, "买方尚未确认接受认购权益（可在线确认或由物业代录线下确认）")
			return
		}
		if t.Mode == "to_resident" {
			var member int
			_ = s.Pool.QueryRow(r.Context(),
				`SELECT count(*) FROM building_members WHERE building_id=$1 AND user_id=$2 AND member_type='resident'`,
				t.Bid, *t.ToID).Scan(&member)
			if member == 0 {
				writeErr(w, 409, "受让人不是本楼栋住户，不能按“转给同楼栋住户”办理")
				return
			}
		}
	}

	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())

	var newInvoiceNo string
	if t.Mode == "with_house" || t.Mode == "to_resident" {
		// 认购过户（状态保持 paid，继续参与后续月份结算）
		if _, err := tx.Exec(r.Context(),
			`UPDATE subscriptions SET user_id=$2 WHERE id=$1`, t.Sid, *t.ToID); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if _, err := tx.Exec(r.Context(),
			`INSERT INTO building_members(building_id,user_id,member_type) VALUES ($1,$2,'resident') ON CONFLICT DO NOTHING`,
			t.Bid, *t.ToID); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO ledger(building_id,user_id,direction,amount,category,ref_type,ref_id,memo)
			VALUES ($1,$2,'in',0,'transfer_in','transfer',$3,'认购权益转入（历史收益仍归原业主）')`,
			t.Bid, *t.ToID, tid); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		// 发票换开：原认购发票作废(replaced)，向新权益人换开
		var oldNo, holder string
		_ = tx.QueryRow(r.Context(),
			`SELECT invoice_no FROM invoices WHERE subscription_id=$1 AND inv_type='subscription' AND status='valid'
			 ORDER BY id LIMIT 1`, t.Sid).Scan(&oldNo)
		if _, err := tx.Exec(r.Context(),
			`UPDATE invoices SET status='replaced', memo=memo||'（房屋转让换开）'
			 WHERE subscription_id=$1 AND inv_type='subscription' AND status='valid'`, t.Sid); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if err := tx.QueryRow(r.Context(), `SELECT name FROM users WHERE id=$1`, *t.ToID).Scan(&holder); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		memo := "认购权益转让换开发票"
		if oldNo != "" {
			memo = "认购权益转让换开发票（原发票 " + oldNo + " 已作废）"
		}
		no, err := s.issueInvoiceTx(r, tx, t.Bid, t.Sid, &tid, *t.ToID, holder, "reissue", subAmount, memo)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		newInvoiceNo = no
	} else {
		// 提前结算退出：认购款退还至卖方收益账户
		if _, err := tx.Exec(r.Context(),
			`UPDATE subscriptions SET status='refunded' WHERE id=$1`, t.Sid); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if subAmount > 0 {
			if _, err := tx.Exec(r.Context(),
				`UPDATE users SET balance=balance+$2 WHERE id=$1`, t.FromID, subAmount); err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			if _, err := tx.Exec(r.Context(),
				`INSERT INTO payments(building_id,user_id,amount,pay_type,memo) VALUES ($1,$2,$3,'refund','提前结算退出-认购款退还至收益账户')`,
				t.Bid, t.FromID, subAmount); err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			if _, err := tx.Exec(r.Context(), `
				INSERT INTO ledger(building_id,user_id,direction,amount,category,ref_type,ref_id,memo)
				VALUES ($1,NULL,'out',$2,'refund','transfer',$3,'提前结算退出退款（公共账户支出）')`,
				t.Bid, subAmount, tid); err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			if _, err := tx.Exec(r.Context(), `
				INSERT INTO ledger(building_id,user_id,direction,amount,category,ref_type,ref_id,memo)
				VALUES ($1,$2,'in',$3,'refund','transfer',$4,'提前结算退出，认购款退还至收益账户')`,
				t.Bid, t.FromID, subAmount, tid); err != nil {
				writeErr(w, 500, err.Error())
				return
			}
		}
		// 发票：原认购发票红冲作废，开具退款凭证
		var oldNo, holder string
		_ = tx.QueryRow(r.Context(),
			`SELECT invoice_no FROM invoices WHERE subscription_id=$1 AND inv_type='subscription' AND status='valid'
			 ORDER BY id LIMIT 1`, t.Sid).Scan(&oldNo)
		if _, err := tx.Exec(r.Context(),
			`UPDATE invoices SET status='voided', memo=memo||'（提前结算退出红冲）'
			 WHERE subscription_id=$1 AND inv_type='subscription' AND status='valid'`, t.Sid); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if err := tx.QueryRow(r.Context(), `SELECT name FROM users WHERE id=$1`, t.FromID).Scan(&holder); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		memo := "提前结算退出退款凭证"
		if oldNo != "" {
			memo = "提前结算退出退款凭证（红冲原认购发票 " + oldNo + "）"
		}
		no, err := s.issueInvoiceTx(r, tx, t.Bid, t.Sid, &tid, t.FromID, holder, "refund", subAmount, memo)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		newInvoiceNo = no
	}

	// 楼栋认购比例刷新（随房/楼内转让不变，退出则下降）
	if _, err := tx.Exec(r.Context(),
		`UPDATE buildings SET subscribed_ratio=(SELECT COALESCE(SUM(panel_ratio),0) FROM subscriptions
		  WHERE building_id=$1 AND status='paid' AND sub_type='shareholder') WHERE id=$1`, t.Bid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(),
		`UPDATE transfers SET status='confirmed', confirmed_at=now(), history_settled=TRUE,
			review_note = review_note||$2 WHERE id=$1`,
		tid, func() string {
			if in.ReviewNote == "" {
				return ""
			}
			return "\n[物业审核] " + in.ReviewNote
		}()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "mode": t.Mode, "invoice_no": newInvoiceNo})
}

func (s *Server) rejectTransfer(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	tid := urlID(r, "tid")
	var in struct {
		Memo string `json:"memo"`
	}
	_ = decodeJSON(r, &in)
	ct, err := s.Pool.Exec(r.Context(),
		`UPDATE transfers SET status='rejected', review_note=review_note||$2 WHERE id=$1 AND status='pending'`,
		tid, "\n[物业驳回] "+in.Memo)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if ct.RowsAffected() == 0 {
		writeErr(w, 404, "转让申请不存在或已处理")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// issueInvoiceTx 事务内开具发票并回写 INV000xxx 发票号
func (s *Server) issueInvoiceTx(r *http.Request, tx pgx.Tx,
	bid, sid int64, tid *int64, uid int64, holder, invType string, amount float64, memo string) (string, error) {
	var id int64
	err := tx.QueryRow(r.Context(), `
		INSERT INTO invoices(invoice_no, building_id, subscription_id, transfer_id, user_id, holder_name, inv_type, amount, memo)
		VALUES ('TMP-' || md5(random()::text), $1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		bid, sid, tid, uid, holder, invType, amount, memo).Scan(&id)
	if err != nil {
		return "", err
	}
	no := fmt.Sprintf("INV%06d", id)
	if _, err := tx.Exec(r.Context(), `UPDATE invoices SET invoice_no=$2 WHERE id=$1`, id, no); err != nil {
		return "", err
	}
	return no, nil
}

// ---------- 交割附件导出 ----------

// exportTransfer 导出房屋交割附件：月度发电、结算、维修扣费、已提现记录（JSON 或 CSV）
func (s *Server) exportTransfer(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	tid := urlID(r, "tid")
	t, status, msg := s.loadTransfer(r, tid)
	if t == nil {
		writeErr(w, status, msg)
		return
	}
	involved := u.ID == t.FromID || (t.ToID != nil && *t.ToID == u.ID) ||
		u.Role == "admin" || u.Role == "property"
	if !involved {
		writeErr(w, 403, "仅买卖双方或物业可导出交割附件")
		return
	}
	ctx := r.Context()

	var buildingName, fromName, fromUnit, disputeNote, memo string
	var ratio, amount float64
	var toName string
	_ = s.Pool.QueryRow(ctx, `SELECT name FROM buildings WHERE id=$1`, t.Bid).Scan(&buildingName)
	_ = s.Pool.QueryRow(ctx, `SELECT name, COALESCE(unit_no,'') FROM users WHERE id=$1`, t.FromID).
		Scan(&fromName, &fromUnit)
	_ = s.Pool.QueryRow(ctx, `SELECT COALESCE(dispute_note,''), COALESCE(memo,'') FROM transfers WHERE id=$1`, tid).
		Scan(&disputeNote, &memo)
	_ = s.Pool.QueryRow(ctx, `SELECT panel_ratio, amount FROM subscriptions WHERE id=$1`, t.Sid).
		Scan(&ratio, &amount)
	if t.ToID != nil {
		_ = s.Pool.QueryRow(ctx, `SELECT name FROM users WHERE id=$1`, *t.ToID).Scan(&toName)
	} else {
		toName = t.ToName
	}

	// 一、月度发电记录
	type genRow struct {
		Month    string
		Inverter string
		Kwh      float64
	}
	gens := []genRow{}
	gRows, err := s.Pool.Query(ctx, `
		SELECT g.period_month, COALESCE(i.name,'楼总表'), g.kwh
		FROM generation_records g LEFT JOIN inverters i ON i.id=g.inverter_id
		WHERE g.building_id=$1 ORDER BY g.period_month, g.id`, t.Bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer gRows.Close()
	for gRows.Next() {
		var g genRow
		if err := gRows.Scan(&g.Month, &g.Inverter, &g.Kwh); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		gens = append(gens, g)
	}

	// 二、卖方户月度结算记录
	type setRow struct {
		Month, Status                                     string
		TotalKwh, Gross, Subsidy                          float64
		Ratio, Revenue, Sub, Om, Tax, Wo, Amount          float64
	}
	settles := []setRow{}
	sRows, err := s.Pool.Query(ctx, `
		SELECT sp.period_month, sp.status, sp.total_kwh, sp.gross_revenue, sp.total_subsidy,
			si.ratio, si.revenue, si.subsidy, si.om_share, si.tax_share, si.wo_share, si.amount
		FROM settlement_items si JOIN settlement_periods sp ON sp.id=si.period_id
		WHERE sp.building_id=$1 AND si.user_id=$2 AND si.item_type='shareholder'
		ORDER BY sp.period_month`, t.Bid, t.FromID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer sRows.Close()
	for sRows.Next() {
		var x setRow
		if err := sRows.Scan(&x.Month, &x.Status, &x.TotalKwh, &x.Gross, &x.Subsidy,
			&x.Ratio, &x.Revenue, &x.Sub, &x.Om, &x.Tax, &x.Wo, &x.Amount); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		settles = append(settles, x)
	}

	// 三、维修扣费（该户各期维修分摊）+ 楼栋工单清单
	type maintRow struct {
		Month string
		Share float64
	}
	maints := []maintRow{}
	for _, x := range settles {
		if x.Wo > 0 {
			maints = append(maints, maintRow{Month: x.Month, Share: x.Wo})
		}
	}
	type woRow struct {
		Month, Title, Resp, Status string
		Cost                       float64
	}
	wos := []woRow{}
	wRows, err := s.Pool.Query(ctx, `
		SELECT period_month, title, responsibility, status, cost FROM work_orders
		WHERE building_id=$1 ORDER BY period_month, id`, t.Bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer wRows.Close()
	for wRows.Next() {
		var x woRow
		if err := wRows.Scan(&x.Month, &x.Title, &x.Resp, &x.Status, &x.Cost); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		wos = append(wos, x)
	}

	// 四、已提现记录（卖方收益账户）
	type wdRow struct {
		At    time.Time
		Amount float64
		Memo  string
	}
	wds := []wdRow{}
	wdRows, err := s.Pool.Query(ctx, `
		SELECT created_at, amount, memo FROM withdrawals WHERE user_id=$1 ORDER BY id`, t.FromID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer wdRows.Close()
	for wdRows.Next() {
		var x wdRow
		if err := wdRows.Scan(&x.At, &x.Amount, &x.Memo); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		wds = append(wds, x)
	}

	// 五、发票记录（该认购相关全部发票）
	type invRow struct {
		No, Type, Holder, Status, Memo string
		Amount                         float64
		At                             time.Time
	}
	invs := []invRow{}
	iRows, err := s.Pool.Query(ctx, `
		SELECT invoice_no, inv_type, holder_name, status, amount, memo, created_at
		FROM invoices WHERE subscription_id=$1 ORDER BY id`, t.Sid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer iRows.Close()
	for iRows.Next() {
		var x invRow
		if err := iRows.Scan(&x.No, &x.Type, &x.Holder, &x.Status, &x.Amount, &x.Memo, &x.At); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		invs = append(invs, x)
	}

	modeLabel := map[string]string{
		"with_house": "随房转让", "early_settlement": "提前结算退出", "to_resident": "转给同楼栋住户",
	}[t.Mode]

	if r.URL.Query().Get("format") == "csv" {
		var buf bytes.Buffer
		buf.WriteString("\xEF\xBB\xBF") // BOM，保证 Excel 打开中文不乱码
		cw := csv.NewWriter(&buf)
		head := func(vals ...string) { _ = cw.Write(vals) }
		head("房屋交割附件（光伏认购权益）")
		head("导出时间", time.Now().Format("2006-01-02 15:04:05"))
		head("楼栋", buildingName)
		head("转让编号", strconv.FormatInt(tid, 10), "处理方式", modeLabel, "状态", t.Status)
		head("卖方", fromName, fromUnit, "认购比例", fmt.Sprintf("%.2f%%", ratio), "认购款", fmt.Sprintf("%.2f", amount))
		head("买方/受让方", toName, t.ToUnit)
		if disputeNote != "" {
			head("争议记录", disputeNote)
		}
		head("")
		head("一、月度发电记录")
		head("月份", "设备", "发电量(kWh)")
		for _, g := range gens {
			head(g.Month, g.Inverter, fmt.Sprintf("%.2f", g.Kwh))
		}
		head("")
		head("二、月度结算记录（卖方户）")
		head("月份", "状态", "全楼发电量(kWh)", "全楼收入(元)", "全楼补贴(元)", "认购比例(%)",
			"发电收益(元)", "补贴分成(元)", "运维分摊(元)", "税费分摊(元)", "维修扣费(元)", "实收(元)")
		for _, x := range settles {
			st := "已定稿"
			if x.Status == "draft" {
				st = "草稿(未结)"
			}
			head(x.Month, st, f2(x.TotalKwh), f2(x.Gross), f2(x.Subsidy), f2(x.Ratio),
				f2(x.Revenue), f2(x.Sub), f2(x.Om), f2(x.Tax), f2(x.Wo), f2(x.Amount))
		}
		head("")
		head("三、维修扣费明细")
		head("月份", "本户维修分摊(元)")
		for _, m := range maints {
			head(m.Month, f2(m.Share))
		}
		head("")
		head("附：楼栋维修工单")
		head("月份", "工单", "责任方", "状态", "费用(元)")
		for _, x := range wos {
			head(x.Month, x.Title, x.Resp, x.Status, f2(x.Cost))
		}
		head("")
		head("四、已提现记录（卖方收益账户）")
		head("时间", "金额(元)", "备注")
		for _, x := range wds {
			head(x.At.Format("2006-01-02 15:04"), f2(x.Amount), x.Memo)
		}
		head("")
		head("五、发票记录")
		head("发票号", "类型", "抬头", "金额(元)", "状态", "开具时间", "备注")
		for _, x := range invs {
			head(x.No, x.Type, x.Holder, f2(x.Amount), x.Status, x.At.Format("2006-01-02 15:04"), x.Memo)
		}
		cw.Flush()
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition",
			fmt.Sprintf("attachment; filename=\"transfer-%d-delivery.csv\"", tid))
		w.WriteHeader(200)
		_, _ = w.Write(buf.Bytes())
		return
	}

	// JSON（默认）
	genOut := []any{}
	for _, g := range gens {
		genOut = append(genOut, map[string]any{"period_month": g.Month, "inverter": g.Inverter, "kwh": g.Kwh})
	}
	setOut := []any{}
	for _, x := range settles {
		setOut = append(setOut, map[string]any{
			"period_month": x.Month, "status": x.Status, "total_kwh": x.TotalKwh,
			"gross_revenue": x.Gross, "total_subsidy": x.Subsidy, "ratio": x.Ratio,
			"revenue": x.Revenue, "subsidy": x.Sub, "om_share": x.Om,
			"tax_share": x.Tax, "wo_share": x.Wo, "amount": x.Amount,
		})
	}
	maintOut := []any{}
	for _, m := range maints {
		maintOut = append(maintOut, map[string]any{"period_month": m.Month, "wo_share": m.Share})
	}
	woOut := []any{}
	for _, x := range wos {
		woOut = append(woOut, map[string]any{
			"period_month": x.Month, "title": x.Title, "responsibility": x.Resp,
			"status": x.Status, "cost": x.Cost,
		})
	}
	wdOut := []any{}
	for _, x := range wds {
		wdOut = append(wdOut, map[string]any{
			"created_at": x.At, "amount": x.Amount, "memo": x.Memo,
		})
	}
	invOut := []any{}
	for _, x := range invs {
		invOut = append(invOut, map[string]any{
			"invoice_no": x.No, "inv_type": x.Type, "holder_name": x.Holder,
			"status": x.Status, "amount": x.Amount, "memo": x.Memo, "created_at": x.At,
		})
	}
	writeJSON(w, 200, map[string]any{
		"title":       "房屋交割附件（光伏认购权益）",
		"exported_at": time.Now(),
		"building":    map[string]any{"id": t.Bid, "name": buildingName},
		"transfer": map[string]any{
			"id": tid, "mode": t.Mode, "mode_label": modeLabel, "status": t.Status,
			"memo": memo, "subscription_id": t.Sid,
		},
		"seller": map[string]any{"id": t.FromID, "name": fromName, "unit_no": fromUnit},
		"buyer":  map[string]any{"name": toName, "unit_no": t.ToUnit},
		"subscription":       map[string]any{"ratio": ratio, "amount": amount},
		"dispute_note":       disputeNote,
		"generation":         genOut,
		"settlements":        setOut,
		"maintenance":        maintOut,
		"work_orders":        woOut,
		"withdrawals":        wdOut,
		"invoices":           invOut,
	})
}

func f2(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// ---------- 发票 ----------

func (s *Server) listInvoices(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	manager := u.Role == "admin" || u.Role == "property"
	rows, err := s.Pool.Query(r.Context(), `
		SELECT i.id, i.invoice_no, COALESCE(i.user_id,0), i.holder_name, i.inv_type, i.amount,
			i.status, COALESCE(i.memo,''), COALESCE(i.subscription_id,0), COALESCE(i.transfer_id,0), i.created_at
		FROM invoices i
		WHERE i.building_id=$1 AND ($2 OR i.user_id=$3)
		ORDER BY i.id DESC LIMIT 300`, bid, manager, u.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var id, uid, sid, tid int64
		var no, holder, typ, status, memo string
		var amount float64
		var at any
		if err := rows.Scan(&id, &no, &uid, &holder, &typ, &amount, &status, &memo, &sid, &tid, &at); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out = append(out, map[string]any{
			"id": id, "invoice_no": no, "user_id": uid, "holder_name": holder,
			"inv_type": typ, "amount": amount, "status": status, "memo": memo,
			"subscription_id": sid, "transfer_id": tid, "created_at": at,
		})
	}
	writeJSON(w, 200, map[string]any{"items": out})
}
