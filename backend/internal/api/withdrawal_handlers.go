package api

import (
	"net/http"
	"strconv"

	"pvplatform/internal/auth"
)

// ---------- 收益账户提现 ----------

// createWithdrawal 住户将收益账户余额提现为现金（记录提现流水）
func (s *Server) createWithdrawal(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	var in struct {
		Amount float64 `json:"amount"`
		Memo   string  `json:"memo"`
	}
	if err := decodeJSON(r, &in); err != nil || in.Amount <= 0 {
		writeErr(w, 400, "提现金额（amount）须大于 0")
		return
	}
	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())
	ct, err := tx.Exec(r.Context(),
		`UPDATE users SET balance=balance-$2 WHERE id=$1 AND balance >= $2`, u.ID, in.Amount)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if ct.RowsAffected() == 0 {
		writeErr(w, 409, "收益账户余额不足，无法提现")
		return
	}
	var id int64
	if err := tx.QueryRow(r.Context(),
		`INSERT INTO withdrawals(user_id, amount, memo) VALUES ($1,$2,$3) RETURNING id`,
		u.ID, in.Amount, in.Memo).Scan(&id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var balance string
	if err := tx.QueryRow(r.Context(), `SELECT balance FROM users WHERE id=$1`, u.ID).Scan(&balance); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "balance": balance})
}

// listWithdrawals 本人提现记录（物业/管理员可用 user_id 查询指定住户）
func (s *Server) listWithdrawals(w http.ResponseWriter, r *http.Request) {
	u := auth.Require(w, r)
	if u == nil {
		return
	}
	target := u.ID
	if q := r.URL.Query().Get("user_id"); q != "" {
		if u.Role != "admin" && u.Role != "property" {
			writeErr(w, 403, "仅物业/管理员可查看他人提现记录")
			return
		}
		if uid, err := strconv.ParseInt(q, 10, 64); err == nil {
			target = uid
		}
	}
	rows, err := s.Pool.Query(r.Context(), `
		SELECT id, amount, memo, created_at FROM withdrawals WHERE user_id=$1 ORDER BY id DESC LIMIT 200`, target)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var id int64
		var amount float64
		var memo string
		var at any
		if err := rows.Scan(&id, &amount, &memo, &at); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out = append(out, map[string]any{"id": id, "amount": amount, "memo": memo, "created_at": at})
	}
	writeJSON(w, 200, map[string]any{"items": out})
}
