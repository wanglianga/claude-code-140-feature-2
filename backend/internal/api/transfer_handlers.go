package api

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"pvplatform/internal/auth"
)

// ---------- 房屋转让 / 认购权益处理（随房转让 / 提前结算退出 / 转给同楼栋住户） ----------

// nextMonth 返回 YYYY-MM 的下一月份
func nextMonth(month string) string {
	if t, err := time.Parse("2006-01", month); err == nil {
		return t.AddDate(0, 1, 0).Format("2006-01")
	}
	return ""
}

// gatherCheck 汇总发起/确认转让前平台必须核查的信息：
// 认购份额、未结收益、维修分摊、买方是否接受权益、历史定稿月份与生效月。
func (s *Server) gatherCheck(ctx context.Context, tx pgx.Tx, bid, sid, toUserID int64) (map[string]any, error) {
	var ratio, amount float64
	var panelCount int
	var subStatus, userName, unitNo string
	var userID int64
	err := tx.QueryRow(ctx, `
		SELECT s.user_id, u.name, COALESCE(u.unit_no,''), s.panel_ratio, s.panel_count, s.amount, s.status
		FROM subscriptions s JOIN users u ON u.id=s.user_id
		WHERE s.id=$1 AND s.building_id=$2`, sid, bid).
		Scan(&userID, &userName, &unitNo, &ratio, &panelCount, &amount, &subStatus)
	if err != nil {
		return nil, err
	}

	var balance float64
	if err := tx.QueryRow(ctx, `SELECT balance FROM users WHERE id=$1`, userID).Scan(&balance); err != nil {
		return nil, err
	}

	// 已定稿历史月份
	var lastFinal string
	_ = tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(period_month),'') FROM settlement_periods WHERE building_id=$1 AND status='finalized'`,
		bid).Scan(&lastFinal)

	// 未定稿（草稿）月份及其住户明细：未入账收益 + 待锁定维修分摊
	draftRows, err := tx.Query(ctx, `
		SELECT sp.period_month, COALESCE(SUM(si.amount) FILTER (WHERE si.user_id=$2),0),
			COALESCE(SUM(si.wo_share) FILTER (WHERE si.user_id=$2),0)
		FROM settlement_periods sp
		LEFT JOIN settlement_items si ON si.period_id=sp.id
		WHERE sp.building_id=$1 AND sp.status='draft'
		GROUP BY sp.period_month ORDER BY sp.period_month`, bid, userID)
	if err != nil {
		return nil, err
	}
	drafts := []any{}
	var draftEarnings, draftWo float64
	for draftRows.Next() {
		var m string
		var amt, wo float64
		if err := draftRows.Scan(&m, &amt, &wo); err != nil {
			draftRows.Close()
			return nil, err
		}
		drafts = append(drafts, map[string]any{"period_month": m, "unposted_amount": amt, "wo_share": wo})
		draftEarnings += amt
		draftWo += wo
	}
	draftRows.Close()

	// 历史（已定稿）维修分摊合计
	var woShareTotal float64
	_ = tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(si.wo_share),0)
		FROM settlement_items si JOIN settlement_periods sp ON sp.id=si.period_id
		WHERE sp.building_id=$1 AND sp.status='finalized' AND si.user_id=$2`, bid, userID).Scan(&woShareTotal)

	// 待处理维修工单（责任/费用尚未锁定时提示）
	var openWO int
	_ = tx.QueryRow(ctx,
		`SELECT count(*) FROM work_orders WHERE building_id=$1 AND status<>'resolved'`, bid).Scan(&openWO)

	check := map[string]any{
		"subscription_id":             sid,
		"seller_user_id":              userID,
		"seller_name":                 userName,
		"seller_unit":                 unitNo,
		"subscription_status":         subStatus,
		"panel_ratio":                 ratio,
		"panel_count":                 panelCount,
		"subscription_amount":         amount,
		"balance":                     balance,       // 收益账户可提现余额（历史已结算）
		"draft_unposted":              draftEarnings, // 草稿期未入账收益
		"draft_periods":               drafts,
		"maintenance_share_finalized": woShareTotal, // 历史维修分摊
		"maintenance_pending_wo":      openWO,       // 未结维修工单数
		"last_finalized_month":        lastFinal,
		"effective_month":             nextMonth(lastFinal), // 物业确认后，下一期按新权益人发放
	}

	if toUserID != 0 {
		var bName, bUnit, bRole string
		var isMember bool
		buyer := map[string]any{"user_id": toUserID}
		err := tx.QueryRow(ctx, `
			SELECT u.name, COALESCE(u.unit_no,''), u.role,
				EXISTS(SELECT 1 FROM building_members bm WHERE bm.building_id=$2 AND bm.user_id=u.id)
			FROM users u WHERE u.id=$1`, toUserID, bid).
			Scan(&bName, &bUnit, &bRole, &isMember)
		if err == nil {
			buyer["name"] = bName
			buyer["unit_no"] = bUnit
			buyer["role"] = bRole
			buyer["is_building_member"] = isMember
			var accepted bool
			var acceptedAt *time.Time
			_ = tx.QueryRow(ctx,
				`SELECT buyer_accepted, buyer_accepted_at FROM transfers
				 WHERE subscription_id=$1 AND to_user_id=$2 AND status='pending'
				 ORDER BY id DESC LIMIT 1`, sid, toUserID).Scan(&accepted, &acceptedAt)
			buyer["accepted"] = accepted
			buyer["accepted_at"] = acceptedAt
		}
		check["buyer"] = buyer
	}

	// 阻断项：物业确认时必须逐条清零
	blockers := []string{}
	if subStatus != "paid" {
		blockers = append(blockers, "认购记录当前状态不是已缴费："+subStatus)
	}
	if len(drafts) > 0 {
		blockers = append(blockers, "存在未定稿月度结算（草稿），历史收益与维修分摊尚未锁定，需先定稿或删除草稿")
	}
	if toUserID != 0 {
		if b, ok := check["buyer"].(map[string]any); ok {
			if _, ok := b["name"]; !ok {
				blockers = append(blockers, "买方账号不存在")
			} else if !b["accepted"].(bool) {
				blockers = append(blockers, "买方尚未在平台确认接受认购权益")
			}
		}
	}
	check["blockers"] = blockers
	return check, nil
}

// transferCheck GET /buildings/{id}/transfer-check?subscription_id=&to_user_id=
func (s *Server) transferCheck(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	sid := queryInt64(r, "subscription_id")
	toID := queryInt64(r, "to_user_id")
	if sid == 0 {
		writeErr(w, 400, "subscription_id 必填")
		return
	}
	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())
	check, err := s.gatherCheck(r.Context(), tx, bid, sid, toID)
	if isNoRows(err) {
		writeErr(w, 404, "认购记录不存在或不属于该楼栋")
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 仅卖方本人、物业/管理员、指定买方可查
	seller := int64(check["seller_user_id"].(int64))
	if u.ID != seller && u.Role != "admin" && u.Role != "property" && u.ID != toID {
		writeErr(w, 403, "只能核查本人的认购权益")
		return
	}
	writeJSON(w, 200, check)
}

func queryInt64(r *http.Request, key string) int64 {
	var v int64
	fmt.Sscanf(r.URL.Query().Get(key), "%d", &v)
	return v
}

// createTransfer 卖方发起认购权益处理：随房转让 / 提前结算退出 / 转给同楼栋住户。
func (s *Server) createTransfer(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	var in struct {
		SubscriptionID int64  `json:"subscription_id"`
		DealType       string `json:"deal_type"` // follow_house / early_exit / transfer_neighbor
		ToUserID       int64  `json:"to_user_id"`
		ToName         string `json:"to_name"`
		ToUnit         string `json:"to_unit"`
		Memo           string `json:"memo"`
	}
	if err := decodeJSON(r, &in); err != nil || in.SubscriptionID == 0 {
		writeErr(w, 400, "subscription_id 必填")
		return
	}
	if in.DealType == "" {
		in.DealType = "follow_house"
	}
	if in.DealType != "follow_house" && in.DealType != "early_exit" && in.DealType != "transfer_neighbor" {
		writeErr(w, 400, "deal_type 必须为 follow_house / early_exit / transfer_neighbor")
		return
	}

	var subBid, fromID int64
	var status string
	err := s.Pool.QueryRow(r.Context(),
		`SELECT building_id, user_id, status FROM subscriptions WHERE id=$1`, in.SubscriptionID).
		Scan(&subBid, &fromID, &status)
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
	if status != "paid" {
		writeErr(w, 409, "仅已缴费的认购可以办理转让/退出，当前状态："+status)
		return
	}
	var pending int
	_ = s.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM transfers WHERE subscription_id=$1 AND status='pending'`, in.SubscriptionID).Scan(&pending)
	if pending > 0 {
		writeErr(w, 409, "该认购已存在办理中的转让申请，请等待物业审核或先驳回")
		return
	}
	if in.DealType != "early_exit" {
		if in.ToUserID == 0 {
			writeErr(w, 400, "受让方需为已在平台注册的住户（to_user_id），由买方登录确认接受权益；可请物业先登记账号")
			return
		}
		var exists, isMember bool
		if err := s.Pool.QueryRow(r.Context(),
			`SELECT TRUE, COALESCE((SELECT TRUE FROM building_members bm
			   WHERE bm.building_id=$1 AND bm.user_id=$2),FALSE)
			 FROM users WHERE id=$2`, bid, in.ToUserID).Scan(&exists, &isMember); err != nil {
			writeErr(w, 400, "受让方账号不存在")
			return
		}
		if in.ToUserID == fromID {
			writeErr(w, 400, "受让方不能是卖方本人")
			return
		}
		if in.DealType == "transfer_neighbor" && !isMember {
			writeErr(w, 409, "转给同楼栋住户时，受让方须为本楼栋住户成员")
			return
		}
	}

	var pendingDraft int
	_ = s.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM settlement_periods WHERE building_id=$1 AND status='draft'`, bid).Scan(&pendingDraft)

	var id int64
	err = s.Pool.QueryRow(r.Context(), `
		INSERT INTO transfers(building_id,subscription_id,from_user_id,to_user_id,to_name,to_unit,
			rights_transfer,refund_requested,deal_type,history_settled,memo)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id`,
		bid, in.SubscriptionID, fromID, nullableID(in.ToUserID), in.ToName, in.ToUnit,
		in.DealType != "early_exit", in.DealType == "early_exit", in.DealType, pendingDraft == 0, in.Memo).Scan(&id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	note := "申请已提交。受让方需在平台确认接受权益，物业审核过户材料后于下一生效月起按新权益人结算。"
	if in.DealType == "early_exit" {
		note = "申请已提交。物业审核过户材料与退出结算后，退还认购款并冲红票据，历史已结算收益仍归本人。"
	}
	writeJSON(w, 201, map[string]any{
		"id": id, "deal_type": in.DealType,
		"history_settled": pendingDraft == 0,
		"note":            note,
	})
}

// acceptTransfer 买方（受让方）确认接受认购权益。
func (s *Server) acceptTransfer(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	tid := urlID(r, "tid")
	var toUser *int64
	var status string
	err := s.Pool.QueryRow(r.Context(),
		`SELECT to_user_id, status FROM transfers WHERE id=$1`, tid).Scan(&toUser, &status)
	if isNoRows(err) {
		writeErr(w, 404, "转让申请不存在")
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if status != "pending" {
		writeErr(w, 409, "该转让申请已处理，不能再确认接受")
		return
	}
	if toUser == nil || *toUser != u.ID {
		writeErr(w, 403, "仅该申请指定的买方可确认接受权益")
		return
	}
	if _, err := s.Pool.Exec(r.Context(),
		`UPDATE transfers SET buyer_accepted=TRUE, buyer_accepted_at=now()
		 WHERE id=$1 AND buyer_accepted=FALSE`, tid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) listTransfers(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	bid := urlID(r, "id")
	rows, err := s.Pool.Query(r.Context(), `
		SELECT t.id, t.subscription_id, f.name, COALESCE(f.unit_no,''),
			COALESCE(t.to_user_id,0), COALESCE(tt.name,t.to_name), COALESCE(tt.unit_no,t.to_unit),
			t.deal_type, t.rights_transfer, t.refund_requested,
			t.buyer_accepted, t.history_settled, t.status,
			COALESCE(t.documents,''), COALESCE(t.documents_note,''), COALESCE(t.review_memo,''),
			COALESCE(t.effective_month,''), COALESCE(t.memo,''), t.created_at, t.confirmed_at,
			COALESCE(rv.name,''), s.panel_ratio
		FROM transfers t
		JOIN users f ON f.id=t.from_user_id
		JOIN subscriptions s ON s.id=t.subscription_id
		LEFT JOIN users tt ON tt.id=t.to_user_id
		LEFT JOIN users rv ON rv.id=t.reviewer_id
		WHERE t.building_id=$1 ORDER BY t.id DESC`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var id, sid, fromID, toID int64
		var fromName, fromUnit, toName, toUnit, dealType, status, memo string
		var documents, docNote, reviewMemo, effectiveMonth, reviewer string
		var rights, refund, accepted, settled bool
		var ratio float64
		var createdAt, confirmedAt any
		if err := rows.Scan(&id, &sid, &fromName, &fromUnit, &toID, &toName, &toUnit,
			&dealType, &rights, &refund, &accepted, &settled, &status,
			&documents, &docNote, &reviewMemo, &effectiveMonth, &memo, &createdAt, &confirmedAt,
			&reviewer, &ratio); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out = append(out, map[string]any{
			"id": id, "subscription_id": sid,
			"from_user_id": fromID, "from_name": fromName, "from_unit": fromUnit,
			"to_user_id": toID, "to_name": toName, "to_unit": toUnit,
			"deal_type": dealType, "rights_transfer": rights, "refund_requested": refund,
			"buyer_accepted": accepted, "history_settled": settled, "status": status,
			"documents": documents, "documents_note": docNote, "review_memo": reviewMemo,
			"effective_month": effectiveMonth, "reviewer": reviewer,
			"panel_ratio": ratio, "memo": memo,
			"created_at": createdAt, "confirmed_at": confirmedAt,
		})
	}
	writeJSON(w, 200, map[string]any{"items": out})
}

// nextInvoiceNo 取平台票据流水号
func nextInvoiceNo(ctx context.Context, tx pgx.Tx) (string, error) {
	var seq int64
	if err := tx.QueryRow(ctx, `SELECT nextval('invoice_seq')`).Scan(&seq); err != nil {
		return "", err
	}
	return fmt.Sprintf("PV-%d", seq), nil
}

func issueInvoice(ctx context.Context, tx pgx.Tx,
	bid int64, invoiceType string, buyerUser *int64, buyerName, item string,
	amount float64, refType string, refID *int64, period string, issuedBy int64, memo string) error {
	return issueInvoiceParent(ctx, tx, bid, invoiceType, nil, buyerUser, buyerName, item,
		amount, refType, refID, period, issuedBy, memo)
}

func issueInvoiceParent(ctx context.Context, tx pgx.Tx,
	bid int64, invoiceType string, parentID, buyerUser *int64, buyerName, item string,
	amount float64, refType string, refID *int64, period string, issuedBy int64, memo string) error {
	no, err := nextInvoiceNo(ctx, tx)
	if err != nil {
		return err
	}
	if buyerName == "" && buyerUser != nil {
		_ = tx.QueryRow(ctx, `SELECT COALESCE(name,'') FROM users WHERE id=$1`, *buyerUser).Scan(&buyerName)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO invoices(building_id,invoice_no,invoice_type,parent_id,buyer_user_id,buyer_name,
			item_name,amount,tax_amount,status,ref_type,ref_id,period_month,issued_by,memo)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,0,'issued',$9,$10,$11,$12,$13)`,
		bid, no, invoiceType, parentID, buyerUser, buyerName, item, amount,
		refType, refID, period, issuedBy, memo)
	return err
}

// confirmTransfer 物业/管理员审核过户材料后确认办理：
//   - follow_house：认购权益随房过户给买方，下一期收益按新权益人发放；开具权益转让凭证；
//   - early_exit：提前结算退出，认购标记 refunded 并退还认购款，冲红原认购票据，楼栋认购比例下调；
//   - transfer_neighbor：认购权益转给同楼栋住户，账户/票据随权益变更。
//
// 历史月份已结算收益始终归原业主（已入其余额），不随权益转移。
func (s *Server) confirmTransfer(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	tid := urlID(r, "tid")
	var in struct {
		Documents     string `json:"documents"` // 过户材料清单（逗号/顿号分隔）
		DocumentsNote string `json:"documents_note"`
		ReviewMemo    string `json:"review_memo"`
	}
	_ = decodeJSON(r, &in)
	if strings.TrimSpace(in.Documents) == "" {
		writeErr(w, 400, "请先核验并登记过户材料（不动产权证/买卖合同、双方身份证明、权益转让协议等）")
		return
	}

	var bid, sid, fromID int64
	var toID *int64
	var toName, toUnit, status, dealType string
	var buyerAccepted bool
	err := s.Pool.QueryRow(r.Context(), `
		SELECT building_id, subscription_id, from_user_id, to_user_id,
			COALESCE(to_name,''), COALESCE(to_unit,''), status, deal_type, buyer_accepted
		FROM transfers WHERE id=$1`, tid).
		Scan(&bid, &sid, &fromID, &toID, &toName, &toUnit, &status, &dealType, &buyerAccepted)
	if isNoRows(err) {
		writeErr(w, 404, "转让申请不存在")
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if status != "pending" {
		writeErr(w, 409, "该转让申请已处理："+status)
		return
	}

	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())

	check, err := s.gatherCheck(r.Context(), tx, bid, sid, deref(toID))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if blockers := check["blockers"].([]string); len(blockers) > 0 {
		writeErr(w, 409, "暂不能确认办理："+strings.Join(blockers, "；"))
		return
	}
	if dealType != "early_exit" && !buyerAccepted {
		writeErr(w, 409, "买方尚未确认接受认购权益")
		return
	}

	var subAmount, ratio float64
	var subStatus string
	if err := tx.QueryRow(r.Context(),
		`SELECT amount, panel_ratio, status FROM subscriptions WHERE id=$1`, sid).Scan(&subAmount, &ratio, &subStatus); err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	effectiveMonth, _ := check["effective_month"].(string)
	sellerName, _ := check["seller_name"].(string)

	switch dealType {
	case "follow_house", "transfer_neighbor":
		newOwner := *toID
		if _, err := tx.Exec(r.Context(),
			`UPDATE subscriptions SET user_id=$2 WHERE id=$1`, sid, newOwner); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if _, err := tx.Exec(r.Context(),
			`INSERT INTO building_members(building_id,user_id,member_type) VALUES ($1,$2,'resident') ON CONFLICT DO NOTHING`,
			bid, newOwner); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO ledger(building_id,user_id,direction,amount,category,ref_type,ref_id,memo)
			VALUES ($1,$2,'in',0,'transfer_in','transfer',$3,$4)`,
			bid, newOwner, tid, transferLedgerMemo(dealType, ratio, effectiveMonth)); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		item := "光伏认购权益随房转让凭证（" + fmt.Sprintf("%.2f%%", ratio) + "，不涉及新现金交易）"
		memo := "权益自 " + effectiveMonth + " 起按新权益人结算；历史已结算收益归原业主"
		if dealType == "transfer_neighbor" {
			item = "光伏认购权益同楼栋转让凭证（" + fmt.Sprintf("%.2f%%", ratio) + "，不涉及新现金交易）"
		}
		if err := issueInvoice(r.Context(), tx, bid, "normal", &newOwner, checkBuyerName(check), item,
			subAmount, "transfer", &tid, "", u.ID, memo); err != nil {
			writeErr(w, 500, err.Error())
			return
		}

	case "early_exit":
		// 提前结算退出：退认购款，历史已结算收益保留在原业主余额（可继续提现）
		if _, err := tx.Exec(r.Context(),
			`UPDATE subscriptions SET status='refunded' WHERE id=$1`, sid); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if subAmount > 0 {
			if _, err := tx.Exec(r.Context(),
				`INSERT INTO payments(building_id,user_id,amount,pay_type,memo) VALUES ($1,$2,$3,'refund','房屋转让-提前结算退出，退还认购款')`,
				bid, fromID, subAmount); err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			if _, err := tx.Exec(r.Context(),
				`INSERT INTO ledger(building_id,user_id,direction,amount,category,ref_type,ref_id,memo)
				 VALUES ($1,$2,'out',$3,'refund','transfer',$4,'提前结算退出，认购款退还')`,
				bid, fromID, subAmount, tid); err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			// 冲红原认购票据（找到该认购最近一张有效蓝字票，关联冲销）
			var origID int64
			var origInvoice *int64
			if e := tx.QueryRow(r.Context(),
				`SELECT id FROM invoices WHERE building_id=$1 AND ref_type='subscription'
				 AND ref_id=$2 AND invoice_type='normal' AND status='issued'
				 ORDER BY id DESC LIMIT 1`, bid, sid).Scan(&origID); e != nil {
				if !isNoRows(e) {
					writeErr(w, 500, e.Error())
					return
				}
			} else {
				origInvoice = &origID
			}
			if err := issueInvoiceParent(r.Context(), tx, bid, "red", origInvoice, &fromID, sellerName,
				"提前结算退出：认购款退回，冲红原认购票据", subAmount,
				"refund", &sid, "", u.ID, "原认购票据全额冲红，退款 "+fmt.Sprintf("%.2f", subAmount)+" 元"); err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			if origInvoice != nil {
				if _, err := tx.Exec(r.Context(), `UPDATE invoices SET status='void' WHERE id=$1`, *origInvoice); err != nil {
					writeErr(w, 500, err.Error())
					return
				}
			}
		}
		if _, err := tx.Exec(r.Context(),
			`UPDATE buildings SET subscribed_ratio=(SELECT COALESCE(SUM(panel_ratio),0) FROM subscriptions
			  WHERE building_id=$1 AND status='paid' AND sub_type='shareholder') WHERE id=$1`, bid); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}

	if _, err := tx.Exec(r.Context(), `
		UPDATE transfers SET status='confirmed', confirmed_at=now(), history_settled=TRUE,
			documents=$2, documents_note=$3, review_memo=$4, reviewer_id=$5, effective_month=$6
		WHERE id=$1`,
		tid, in.Documents, in.DocumentsNote, in.ReviewMemo, u.ID, effectiveMonth); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "deal_type": dealType, "effective_month": effectiveMonth,
		"note": "材料审核通过，自 " + effectiveMonth + " 起下一期收益按新权益人发放",
	})
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func checkBuyerName(check map[string]any) string {
	if b, ok := check["buyer"].(map[string]any); ok {
		if n, ok := b["name"].(string); ok {
			return n
		}
	}
	return ""
}

func transferLedgerMemo(dealType string, ratio float64, effectiveMonth string) string {
	if dealType == "transfer_neighbor" {
		return fmt.Sprintf("认购权益（%.2f%%）转给同楼栋住户，%s 起按新权益人结算", ratio, effectiveMonth)
	}
	return fmt.Sprintf("认购权益随房屋转入（%.2f%%），%s 起按新权益人结算", ratio, effectiveMonth)
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
		`UPDATE transfers SET status='rejected', memo=COALESCE(NULLIF($2,''),memo) WHERE id=$1 AND status='pending'`,
		tid, in.Memo)
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

// transferExport 导出历史收益交割附件（CSV）：
// 月度发电、月度结算（收入/补贴/运维/税/维修扣费/住户净额）、维修扣费明细、已提现记录、票据。
func (s *Server) transferExport(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	sid := queryInt64(r, "subscription_id")
	if sid == 0 {
		writeErr(w, 400, "subscription_id 必填")
		return
	}
	var curID int64
	var curName, unit string
	var ratio, subAmount float64
	err := s.Pool.QueryRow(r.Context(), `
		SELECT s.user_id, u.name, COALESCE(u.unit_no,''), s.panel_ratio, s.amount
		FROM subscriptions s JOIN users u ON u.id=s.user_id
		WHERE s.id=$1 AND s.building_id=$2`, sid, bid).Scan(&curID, &curName, &unit, &ratio, &subAmount)
	if isNoRows(err) {
		writeErr(w, 404, "认购记录不存在或不属于该楼栋")
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 历任权益人：当前持有人 + 转让记录中的出让/受让双方
	pRows, err := s.Pool.Query(r.Context(), `
		SELECT user_id FROM (
			SELECT s.user_id FROM subscriptions s WHERE s.id=$1
			UNION
			SELECT from_user_id FROM transfers WHERE subscription_id=$1
			UNION
			SELECT to_user_id FROM transfers WHERE subscription_id=$1 AND to_user_id IS NOT NULL
		) t WHERE user_id IS NOT NULL`, sid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var holderIDs []int64
	for pRows.Next() {
		var id int64
		if err := pRows.Scan(&id); err != nil {
			pRows.Close()
			writeErr(w, 500, err.Error())
			return
		}
		holderIDs = append(holderIDs, id)
	}
	pRows.Close()
	allowed := u.Role == "admin" || u.Role == "property"
	for _, id := range holderIDs {
		if u.ID == id {
			allowed = true
		}
	}
	if !allowed {
		writeErr(w, 403, "只能导出本人（或本户历任权益人）认购的交割附件")
		return
	}
	holderNames := map[int64]string{}
	for _, id := range holderIDs {
		var nm string
		_ = s.Pool.QueryRow(r.Context(), `SELECT name FROM users WHERE id=$1`, id).Scan(&nm)
		holderNames[id] = nm
	}

	var bName string
	_ = s.Pool.QueryRow(r.Context(), `SELECT name FROM buildings WHERE id=$1`, bid).Scan(&bName)

	cw := csv.NewWriter(w)
	header := func(title string) {
		_ = cw.Write([]string{title})
		_ = cw.Write(nil)
	}
	row := func(vals ...string) { _ = cw.Write(vals) }

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="transfer_settlement_%s.csv"`, unit))
	// Excel 兼容 UTF-8 BOM
	_ = cw.Write([]string{"\ufeff光伏认购权益房屋交割附件（历史收益对账单）"})
	_ = cw.Write(nil)
	row("楼栋", bName)
	row("房号", unit, "当前权益人", curName)
	row("认购比例(%)", fmt.Sprintf("%.2f", ratio), "认购金额(元)", fmt.Sprintf("%.2f", subAmount))
	parties := ""
	for i, id := range holderIDs {
		if i > 0 {
			parties += "、"
		}
		parties += holderNames[id]
		var bal, wdTotal float64
		_ = s.Pool.QueryRow(r.Context(), `SELECT balance FROM users WHERE id=$1`, id).Scan(&bal)
		_ = s.Pool.QueryRow(r.Context(),
			`SELECT COALESCE(SUM(amount),0) FROM withdrawals WHERE building_id=$1 AND user_id=$2 AND status='paid'`,
			bid, id).Scan(&wdTotal)
		parties += fmt.Sprintf("(余额%.2f/已提现%.2f)", bal, wdTotal)
	}
	row("历任权益人", parties)
	row("导出时间", time.Now().Format("2006-01-02 15:04:05"))
	_ = cw.Write(nil)

	// 1. 月度发电
	header("一、月度发电量（kWh）")
	row("月份", "总发电量(kWh)", "上网电价(元/kWh)", "度电补贴(元/kWh)", "上网电费收入(元)", "补贴(元)", "状态")
	gRows, err := s.Pool.Query(r.Context(), `
		SELECT period_month,total_kwh,grid_price,subsidy_per_kwh,gross_revenue,total_subsidy,status
		FROM settlement_periods WHERE building_id=$1 ORDER BY period_month`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for gRows.Next() {
		var m, st string
		var kwh, price, subRate, rev, sub float64
		if err := gRows.Scan(&m, &kwh, &price, &subRate, &rev, &sub, &st); err != nil {
			gRows.Close()
			writeErr(w, 500, err.Error())
			return
		}
		row(m, f2(kwh), f2(price), f2(subRate), f2(rev), f2(sub),
			map[string]string{"draft": "草稿", "finalized": "已定稿"}[st])
	}
	gRows.Close()
	_ = cw.Write(nil)

	// 2. 历任权益人月度结算明细
	header("二、月度结算明细（该认购历任权益人）")
	row("住户", "月份", "状态", "分成电量(kWh)", "电费收入(元)", "补贴分成(元)", "运维分摊(元)", "税费分摊(元)", "维修扣费(元)", "当月净收益(元)")
	sRows, err := s.Pool.Query(r.Context(), `
		SELECT COALESCE(u.name,''), sp.period_month, sp.status, si.kwh, si.revenue, si.subsidy,
			si.om_share, si.tax_share, si.wo_share, si.amount
		FROM settlement_items si
		JOIN settlement_periods sp ON sp.id=si.period_id
		JOIN users u ON u.id=si.user_id
		WHERE sp.building_id=$1 AND si.item_type='shareholder' AND si.user_id = ANY($2)
		ORDER BY sp.period_month, u.name`, bid, holderIDs)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	netTotal := map[string]float64{}
	for sRows.Next() {
		var name, m, st string
		var kwh, rev, sub, om, tax, wo, amt float64
		if err := sRows.Scan(&name, &m, &st, &kwh, &rev, &sub, &om, &tax, &wo, &amt); err != nil {
			sRows.Close()
			writeErr(w, 500, err.Error())
			return
		}
		if st == "finalized" {
			netTotal[name] += amt
		}
		row(name, m, map[string]string{"draft": "草稿", "finalized": "已定稿"}[st],
			f2(kwh), f2(rev), f2(sub), f2(om), f2(tax), f2(wo), f2(amt))
	}
	sRows.Close()
	for name, v := range netTotal {
		row(name+" 已定稿累计净收益(元)", f2(v))
	}
	_ = cw.Write(nil)

	// 3. 维修扣费（按各权益人分摊）
	header("三、维修扣费（维修基金不足、按组件比例分摊部分）")
	row("月份", "工单", "类型", "责任方", "工单费用(元)", "分摊住户", "住户分摊(元)", "工单状态")
	mRows, err := s.Pool.Query(r.Context(), `
		SELECT wo.period_month, wo.title, wo.wo_type, wo.responsibility, wo.cost, wo.status,
			COALESCE(u.name,''), COALESCE(si.wo_share,0)
		FROM work_orders wo
		JOIN settlement_periods sp ON sp.building_id=wo.building_id AND sp.period_month=wo.period_month AND sp.status='finalized'
		JOIN settlement_items si ON si.period_id=sp.id AND si.item_type='shareholder' AND si.wo_share > 0
		JOIN users u ON u.id=si.user_id
		WHERE wo.building_id=$1 AND si.user_id = ANY($2)
		ORDER BY wo.period_month, wo.id, u.name`, bid, holderIDs)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	hasWO := false
	for mRows.Next() {
		var m, title, wtype, resp, wstat, name string
		var cost, share float64
		if err := mRows.Scan(&m, &title, &wtype, &resp, &cost, &wstat, &name, &share); err != nil {
			mRows.Close()
			writeErr(w, 500, err.Error())
			return
		}
		hasWO = true
		row(m, title, woTypeZh(wtype), respZh(resp), f2(cost), name, f2(share), woStatusZh(wstat))
	}
	mRows.Close()
	if !hasWO {
		row("（无）")
	}
	_ = cw.Write(nil)

	// 4. 已提现记录
	header("四、已提现记录")
	row("住户", "申请时间", "提现金额(元)", "状态", "收款账户", "放款时间")
	tRows, err := s.Pool.Query(r.Context(), `
		SELECT COALESCE(u.name,''), to_char(wd.created_at,'YYYY-MM-DD HH24:MI'), wd.amount, wd.status,
			COALESCE(wd.bank_info,''), COALESCE(to_char(wd.paid_at,'YYYY-MM-DD HH24:MI'),'')
		FROM withdrawals wd JOIN users u ON u.id=wd.user_id
		WHERE wd.building_id=$1 AND wd.user_id = ANY($2) ORDER BY wd.id`, bid, holderIDs)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	hasWD := false
	for tRows.Next() {
		var name, created, amt, st, bank, paid string
		if err := tRows.Scan(&name, &created, &amt, &st, &bank, &paid); err != nil {
			tRows.Close()
			writeErr(w, 500, err.Error())
			return
		}
		hasWD = true
		row(name, created, amt, withdrawZh(st), bank, paid)
	}
	tRows.Close()
	if !hasWD {
		row("（无）")
	}
	_ = cw.Write(nil)

	// 5. 票据记录
	header("五、票据记录（认购/收益/退款/转让）")
	row("开票时间", "票据号码", "类型", "抬头", "项目内容", "金额(元)", "业务", "期间", "状态", "备注")
	iRows, err := s.Pool.Query(r.Context(), `
		SELECT to_char(created_at,'YYYY-MM-DD HH24:MI'), invoice_no, invoice_type, COALESCE(buyer_name,''),
			item_name, amount, ref_type, COALESCE(period_month,''), status, COALESCE(memo,'')
		FROM invoices WHERE building_id=$1 AND buyer_user_id = ANY($2) ORDER BY id`, bid, holderIDs)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	hasInv := false
	for iRows.Next() {
		var created, no, itype, buyer, item, amt, ref, period, st, memo string
		if err := iRows.Scan(&created, &no, &itype, &buyer, &item, &amt, &ref, &period, &st, &memo); err != nil {
			iRows.Close()
			writeErr(w, 500, err.Error())
			return
		}
		hasInv = true
		row(created, no, map[string]string{"normal": "蓝字", "red": "红字冲销"}[itype], buyer,
			item, amt, refZh(ref), period,
			map[string]string{"issued": "已开具", "void": "已作废"}[st], memo)
	}
	iRows.Close()
	if !hasInv {
		row("（无）")
	}

	cw.Flush()
}

func f2(v float64) string { return fmt.Sprintf("%.2f", v) }

func woTypeZh(t string) string {
	return map[string]string{"leak": "漏水", "inverter": "逆变器故障", "channel": "维护通道", "other": "其他"}[t]
}
func respZh(t string) string {
	return map[string]string{"undetermined": "待判定", "property": "物业", "installer": "安装商", "owner": "业主", "insurance": "保险"}[t]
}
func woStatusZh(t string) string {
	return map[string]string{"open": "待处理", "processing": "处理中", "resolved": "已解决"}[t]
}
func withdrawZh(t string) string {
	return map[string]string{"pending": "待审核", "paid": "已放款", "rejected": "已驳回"}[t]
}
func refZh(t string) string {
	return map[string]string{
		"subscription": "认购缴费", "settlement": "月度收益", "refund": "退款冲红", "transfer": "权益转让",
	}[t]
}
