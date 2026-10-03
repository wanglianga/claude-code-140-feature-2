package api

import (
	"net/http"
	"strconv"

	"pvplatform/internal/auth"
)

// ---------- 发票/票据 ----------

func (s *Server) listInvoices(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	isManager := u.Role == "admin" || u.Role == "property"
	// 住户默认只看本人票据；物业/管理员可看全部，也可用 user_id 过滤
	uidFilter := queryInt64(r, "user_id")
	args := []any{bid}
	where := "building_id=$1"
	if !isManager {
		where += " AND buyer_user_id=$2"
		args = append(args, u.ID)
	} else if uidFilter != 0 {
		where += " AND buyer_user_id=$2"
		args = append(args, uidFilter)
	}
	rows, err := s.Pool.Query(r.Context(), `
		SELECT id, invoice_no, invoice_type, COALESCE(parent_id,0), COALESCE(buyer_user_id,0),
			COALESCE(buyer_name,''), item_name, amount, tax_amount, status,
			ref_type, COALESCE(ref_id,0), COALESCE(period_month,''), COALESCE(memo,''), created_at
		FROM invoices WHERE `+where+` ORDER BY id DESC LIMIT 300`, args...)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var id, parentID, buyerID, refID int64
		var no, itype, buyer, item, status, refType, period, memo string
		var amount, tax float64
		var at any
		if err := rows.Scan(&id, &no, &itype, &parentID, &buyerID, &buyer, &item,
			&amount, &tax, &status, &refType, &refID, &period, &memo, &at); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out = append(out, map[string]any{
			"id": id, "invoice_no": no, "invoice_type": itype, "parent_id": parentID,
			"buyer_user_id": buyerID, "buyer_name": buyer, "item_name": item,
			"amount": amount, "tax_amount": tax, "status": status,
			"ref_type": refType, "ref_id": refID, "period_month": period, "memo": memo,
			"created_at": at,
		})
	}
	writeJSON(w, 200, map[string]any{"items": out})
}

// voidInvoice 作废蓝字票据（仅管理端），同时补开一张等额红字冲销凭证。
func (s *Server) voidInvoice(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	iid := urlID(r, "iid")
	var bid int64
	var no, itype, item, buyer, refType, period, status string
	var amount float64
	var buyerID *int64
	err := s.Pool.QueryRow(r.Context(), `
		SELECT building_id, invoice_no, invoice_type, buyer_user_id, COALESCE(buyer_name,''),
			item_name, amount, status, ref_type, COALESCE(period_month,'')
		FROM invoices WHERE id=$1`, iid).
		Scan(&bid, &no, &itype, &buyerID, &buyer, &item, &amount, &status, &refType, &period)
	if isNoRows(err) {
		writeErr(w, 404, "票据不存在")
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if itype == "red" {
		writeErr(w, 409, "红字冲销凭证不能再冲销")
		return
	}
	if status == "void" {
		writeErr(w, 409, "票据已作废")
		return
	}
	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `UPDATE invoices SET status='void' WHERE id=$1`, iid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	redNo, err := nextInvoiceNo(r.Context(), tx)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO invoices(building_id,invoice_no,invoice_type,parent_id,buyer_user_id,buyer_name,
			item_name,amount,tax_amount,status,ref_type,ref_id,period_month,issued_by,memo)
		VALUES ($1,$2,'red',$3,$4,$5,$6,$7,0,'issued',$8,$9,$10,$11,'冲销原票据 '||$12)`,
		bid, redNo, iid, buyerID, buyer, item, amount, refType, &iid, period, u.ID, no); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "red_invoice_no": redNo})
}

// ---------- 收益提现 ----------

func (s *Server) createWithdrawal(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	var in struct {
		Amount   float64 `json:"amount"`
		BankInfo string  `json:"bank_info"`
	}
	if err := decodeJSON(r, &in); err != nil || in.Amount <= 0 {
		writeErr(w, 400, "提现金额必须大于 0")
		return
	}
	// 仅可提现本楼栋产生的可提余额：取该住户在本楼已定稿结算净额，减去已成功/审核中的提现与已发生退款
	var earnTotal float64
	_ = s.Pool.QueryRow(r.Context(), `
		SELECT COALESCE(SUM(si.amount),0)
		FROM settlement_items si
		JOIN settlement_periods sp ON sp.id=si.period_id
		WHERE sp.building_id=$1 AND sp.status='finalized'
		  AND si.item_type='shareholder' AND si.user_id=$2`, bid, u.ID).Scan(&earnTotal)
	var withdrawn float64
	_ = s.Pool.QueryRow(r.Context(),
		`SELECT COALESCE(SUM(amount),0) FROM withdrawals
		 WHERE building_id=$1 AND user_id=$2 AND status IN ('pending','paid')`, bid, u.ID).Scan(&withdrawn)
	available := earnTotal - withdrawn
	if in.Amount > available+0.001 {
		writeErr(w, 409, "可提余额不足（本楼栋可提 "+f2(available)+" 元，含审核中的提现）")
		return
	}
	var id int64
	err := s.Pool.QueryRow(r.Context(), `
		INSERT INTO withdrawals(building_id,user_id,amount,bank_info) VALUES ($1,$2,$3,$4) RETURNING id`,
		bid, u.ID, in.Amount, in.BankInfo).Scan(&id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "available": available})
}

func (s *Server) listWithdrawals(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	isManager := u.Role == "admin" || u.Role == "property"
	args := []any{bid}
	where := "wd.building_id=$1"
	if !isManager {
		where += " AND wd.user_id=$2"
		args = append(args, u.ID)
	}
	rows, err := s.Pool.Query(r.Context(), `
		SELECT wd.id, wd.user_id, COALESCE(u.name,''), wd.amount, wd.status,
			COALESCE(wd.bank_info,''), COALESCE(wd.reject_memo,''), wd.created_at,
			COALESCE(to_char(wd.paid_at,'YYYY-MM-DD HH24:MI'),'')
		FROM withdrawals wd JOIN users u ON u.id=wd.user_id
		WHERE `+where+` ORDER BY wd.id DESC LIMIT 200`, args...)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []any{}
	var pendingTotal float64
	for rows.Next() {
		var id, uid int64
		var name, status, bank, memo string
		var amount float64
		var createdAt any
		var paidStr string
		if err := rows.Scan(&id, &uid, &name, &amount, &status, &bank, &memo, &createdAt, &paidStr); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if status == "pending" {
			pendingTotal += amount
		}
		out = append(out, map[string]any{
			"id": id, "user_id": uid, "name": name, "amount": amount, "status": status,
			"bank_info": bank, "reject_memo": memo, "created_at": createdAt, "paid_at": paidStr,
		})
	}

	// 本楼栋各住户可提余额（管理端展示）
	availableMap := map[string]float64{}
	if isManager {
		erows, err := s.Pool.Query(r.Context(), `
			SELECT si.user_id, COALESCE(SUM(si.amount),0)
			FROM settlement_items si
			JOIN settlement_periods sp ON sp.id=si.period_id
			WHERE sp.building_id=$1 AND sp.status='finalized' AND si.item_type='shareholder'
			GROUP BY si.user_id`, bid)
		if err == nil {
			type kv struct {
				uid int64
				v   float64
			}
			var earns []kv
			for erows.Next() {
				var k kv
				_ = erows.Scan(&k.uid, &k.v)
				earns = append(earns, k)
			}
			erows.Close()
			for _, k := range earns {
				var wd float64
				_ = s.Pool.QueryRow(r.Context(),
					`SELECT COALESCE(SUM(amount),0) FROM withdrawals
					 WHERE building_id=$1 AND user_id=$2 AND status IN ('pending','paid')`,
					bid, k.uid).Scan(&wd)
				availableMap[strconv.FormatInt(k.uid, 10)] = k.v - wd
			}
		}
	} else {
		var earnTotal, withdrawn float64
		_ = s.Pool.QueryRow(r.Context(), `
			SELECT COALESCE(SUM(si.amount),0)
			FROM settlement_items si JOIN settlement_periods sp ON sp.id=si.period_id
			WHERE sp.building_id=$1 AND sp.status='finalized'
			  AND si.item_type='shareholder' AND si.user_id=$2`, bid, u.ID).Scan(&earnTotal)
		_ = s.Pool.QueryRow(r.Context(),
			`SELECT COALESCE(SUM(amount),0) FROM withdrawals
			 WHERE building_id=$1 AND user_id=$2 AND status IN ('pending','paid')`, bid, u.ID).Scan(&withdrawn)
		availableMap["me"] = earnTotal - withdrawn
	}

	writeJSON(w, 200, map[string]any{
		"items": out, "pending_total": pendingTotal, "available": availableMap,
	})
}

// reviewWithdrawal 物业审核提现：paid 放款并扣减余额、记台账；rejected 退回额度。
func (s *Server) reviewWithdrawal(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	wid := urlID(r, "wid")
	var in struct {
		Approve bool   `json:"approve"`
		Memo    string `json:"memo"`
	}
	_ = decodeJSON(r, &in)

	var bid, uid int64
	var amount float64
	var status string
	err := s.Pool.QueryRow(r.Context(),
		`SELECT building_id,user_id,amount,status FROM withdrawals WHERE id=$1`, wid).
		Scan(&bid, &uid, &amount, &status)
	if isNoRows(err) {
		writeErr(w, 404, "提现申请不存在")
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if status != "pending" {
		writeErr(w, 409, "该提现申请已处理："+status)
		return
	}

	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())

	if !in.Approve {
		if _, err := tx.Exec(r.Context(),
			`UPDATE withdrawals SET status='rejected', reject_memo=$2, reviewer_id=$3 WHERE id=$1`,
			wid, in.Memo, u.ID); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}

	// 放款前再次校验住户总余额（拒绝透支）
	var balance float64
	if err := tx.QueryRow(r.Context(), `SELECT balance FROM users WHERE id=$1`, uid).Scan(&balance); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if amount > balance+0.001 {
		writeErr(w, 409, "住户收益账户余额不足，无法放款（余额 "+f2(balance)+" 元）")
		return
	}
	if _, err := tx.Exec(r.Context(),
		`UPDATE users SET balance=balance-$2 WHERE id=$1`, uid, amount); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO ledger(building_id,user_id,direction,amount,category,ref_type,ref_id,memo)
		VALUES ($1,$2,'out',$3,'withdrawal','withdrawal',$4,'收益提现放款')`,
		bid, uid, amount, wid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(),
		`UPDATE withdrawals SET status='paid', paid_at=now(), reviewer_id=$2,
			reject_memo=COALESCE(NULLIF($3,''),reject_memo) WHERE id=$1`,
		wid, u.ID, in.Memo); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
