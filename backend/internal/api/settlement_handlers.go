package api

import (
	"encoding/json"
	"net/http"

	"pvplatform/internal/auth"
	"pvplatform/internal/settlement"
)

// fundBalance 维修基金余额 = 计提(in) - 动用(out)
func (s *Server) fundBalance(r *http.Request, bid int64) float64 {
	var bal float64
	_ = s.Pool.QueryRow(r.Context(), `
		SELECT COALESCE(SUM(CASE WHEN direction='in' THEN amount ELSE -amount END),0)
		FROM ledger WHERE building_id=$1 AND category='maintenance_fund'`, bid).Scan(&bal)
	return bal
}

// createSettlement 按实际发电量与规则生成（或重新生成草稿）月度结算
func (s *Server) createSettlement(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	bid := urlID(r, "id")
	var in struct {
		PeriodMonth  string  `json:"period_month"`
		GridPrice    float64 `json:"grid_price"`
		SubsidyRate  float64 `json:"subsidy_per_kwh"`
		OmFee        float64 `json:"om_fee"`
		Tax          float64 `json:"tax"`
		FundRatio    float64 `json:"maintenance_fund_ratio"`
		Compensation float64 `json:"compensation"`
	}
	if err := decodeJSON(r, &in); err != nil || in.PeriodMonth == "" {
		writeErr(w, 400, "period_month（YYYY-MM）必填")
		return
	}
	// 已有定稿则禁止覆盖
	var existingStatus string
	_ = s.Pool.QueryRow(r.Context(),
		`SELECT status FROM settlement_periods WHERE building_id=$1 AND period_month=$2`,
		bid, in.PeriodMonth).Scan(&existingStatus)
	if existingStatus == "finalized" {
		writeErr(w, 409, "该月结算已定稿，不可重新生成")
		return
	}

	// 发电量：优先楼总表记录，否则取逆变器分表之和
	var totalKwh float64
	err := s.Pool.QueryRow(r.Context(),
		`SELECT kwh FROM generation_records WHERE building_id=$1 AND period_month=$2 AND inverter_id IS NULL`,
		bid, in.PeriodMonth).Scan(&totalKwh)
	if isNoRows(err) {
		if e := s.Pool.QueryRow(r.Context(),
			`SELECT COALESCE(SUM(kwh),0) FROM generation_records WHERE building_id=$1 AND period_month=$2`,
			bid, in.PeriodMonth).Scan(&totalKwh); e != nil {
			writeErr(w, 500, e.Error())
			return
		}
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if totalKwh <= 0 {
		writeErr(w, 409, "该月没有发电量记录，无法结算")
		return
	}

	// 已认购住户份额（paid 或已随房转移）
	rows, err := s.Pool.Query(r.Context(), `
		SELECT s.user_id, u.name, s.panel_ratio
		FROM subscriptions s JOIN users u ON u.id=s.user_id
		WHERE s.building_id=$1 AND s.sub_type='shareholder'
		  AND s.status IN ('paid','transferred')
		ORDER BY s.id`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	var shares []settlement.Share
	for rows.Next() {
		var sh settlement.Share
		if err := rows.Scan(&sh.UserID, &sh.Name, &sh.Ratio); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		shares = append(shares, sh)
	}

	// 当期工单：费用 >0 且责任已判定的才进入结算
	woRows, err := s.Pool.Query(r.Context(), `
		SELECT id, title, cost, responsibility FROM work_orders
		WHERE building_id=$1 AND period_month=$2 AND cost > 0 AND responsibility <> 'undetermined'`,
		bid, in.PeriodMonth)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer woRows.Close()
	var wos []settlement.WorkOrder
	for woRows.Next() {
		var wo settlement.WorkOrder
		if err := woRows.Scan(&wo.ID, &wo.Title, &wo.Cost, &wo.Responsibility); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		wos = append(wos, wo)
	}

	if in.FundRatio <= 0 {
		var ruleJSON string
		_ = s.Pool.QueryRow(r.Context(), `SELECT public_revenue_rule::text FROM buildings WHERE id=$1`, bid).Scan(&ruleJSON)
		var rule struct {
			MaintenanceFundRatio float64 `json:"maintenance_fund_ratio"`
		}
		_ = json.Unmarshal([]byte(ruleJSON), &rule)
		in.FundRatio = rule.MaintenanceFundRatio
		if in.FundRatio <= 0 {
			in.FundRatio = 20
		}
	}

	res := settlement.Compute(settlement.Input{
		BuildingID:   bid,
		PeriodMonth:  in.PeriodMonth,
		TotalKwh:     totalKwh,
		GridPrice:    in.GridPrice,
		SubsidyRate:  in.SubsidyRate,
		OmFee:        in.OmFee,
		Tax:          in.Tax,
		FundRatio:    in.FundRatio,
		FundBalance:  s.fundBalance(r, bid),
		Compensation: in.Compensation,
		Shareholders: shares,
		WorkOrders:   wos,
	})

	var periodID int64
	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())
	err = tx.QueryRow(r.Context(), `
		INSERT INTO settlement_periods(building_id,period_month,total_kwh,grid_price,subsidy_per_kwh,
			gross_revenue,total_subsidy,om_fee,tax,work_order_cost,maintenance_fund_ratio,
			maintenance_fund_used,maintenance_fund_addition,compensation,public_ratio,
			distributable,created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		ON CONFLICT (building_id,period_month) DO UPDATE SET
			total_kwh=EXCLUDED.total_kwh, grid_price=EXCLUDED.grid_price,
			subsidy_per_kwh=EXCLUDED.subsidy_per_kwh, gross_revenue=EXCLUDED.gross_revenue,
			total_subsidy=EXCLUDED.total_subsidy, om_fee=EXCLUDED.om_fee, tax=EXCLUDED.tax,
			work_order_cost=EXCLUDED.work_order_cost, maintenance_fund_ratio=EXCLUDED.maintenance_fund_ratio,
			maintenance_fund_used=EXCLUDED.maintenance_fund_used,
			maintenance_fund_addition=EXCLUDED.maintenance_fund_addition,
			compensation=EXCLUDED.compensation,
			public_ratio=EXCLUDED.public_ratio, distributable=EXCLUDED.distributable
		RETURNING id`,
		bid, in.PeriodMonth, res.TotalKwh, in.GridPrice, in.SubsidyRate,
		res.GrossRevenue, res.TotalSubsidy, res.OmFee, res.Tax, res.WorkOrderCost,
		in.FundRatio, res.FundUsed, res.FundAddition, res.Compensation, res.PublicRatio,
		res.Distributable, u.ID).Scan(&periodID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(), `DELETE FROM settlement_items WHERE period_id=$1`, periodID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, it := range res.Items {
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO settlement_items(period_id,user_id,item_type,ratio,kwh,revenue,subsidy,om_share,tax_share,wo_share,amount,memo)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			periodID, it.UserID, it.ItemType, it.Ratio, it.Kwh, it.Revenue, it.Subsidy,
			it.OmShare, it.TaxShare, it.WoShare, it.Amount, it.Memo); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": periodID, "settlement": res})
}

func (s *Server) listSettlements(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	bid := urlID(r, "id")
	rows, err := s.Pool.Query(r.Context(), `
		SELECT id, period_month, status, total_kwh, grid_price, subsidy_per_kwh,
			gross_revenue, total_subsidy, om_fee, tax, work_order_cost,
			maintenance_fund_ratio, maintenance_fund_used, maintenance_fund_addition,
			compensation, public_ratio, distributable, created_at, finalized_at
		FROM settlement_periods WHERE building_id=$1 ORDER BY period_month DESC`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var id int64
		var month, status string
		var kwh, price, subsidy, gross, totalSub, om, tax, woCost float64
		var fundRatio, fundUsed, fundAdd, comp, pubRatio, dist float64
		var createdAt, finalizedAt any
		if err := rows.Scan(&id, &month, &status, &kwh, &price, &subsidy, &gross, &totalSub,
			&om, &tax, &woCost, &fundRatio, &fundUsed, &fundAdd, &comp, &pubRatio, &dist,
			&createdAt, &finalizedAt); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out = append(out, map[string]any{
			"id": id, "period_month": month, "status": status, "total_kwh": kwh,
			"grid_price": price, "subsidy_per_kwh": subsidy, "gross_revenue": gross,
			"total_subsidy": totalSub, "om_fee": om, "tax": tax, "work_order_cost": woCost,
			"maintenance_fund_ratio": fundRatio, "maintenance_fund_used": fundUsed,
			"maintenance_fund_addition": fundAdd, "compensation": comp,
			"public_ratio":  pubRatio,
			"distributable": dist, "created_at": createdAt, "finalized_at": finalizedAt,
		})
	}
	writeJSON(w, 200, map[string]any{"items": out})
}

func (s *Server) getSettlement(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	sid := urlID(r, "sid")
	var bid int64
	var month, status string
	var kwh, price, subRate, gross, totalSub, om, tax, woCost, fundRatio, pubRatio, dist float64
	var fundUsed, fundAdd, compAmt float64
	var finalizedAt any
	err := s.Pool.QueryRow(r.Context(), `
		SELECT building_id, period_month, status, total_kwh, grid_price, subsidy_per_kwh,
			gross_revenue, total_subsidy, om_fee, tax, work_order_cost,
			maintenance_fund_ratio, maintenance_fund_used, maintenance_fund_addition,
			compensation, public_ratio, distributable, finalized_at
		FROM settlement_periods WHERE id=$1`, sid).
		Scan(&bid, &month, &status, &kwh, &price, &subRate, &gross, &totalSub,
			&om, &tax, &woCost, &fundRatio, &fundUsed, &fundAdd, &compAmt, &pubRatio, &dist, &finalizedAt)
	if isNoRows(err) {
		writeErr(w, 404, "结算单不存在")
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	rows, err := s.Pool.Query(r.Context(), `
		SELECT si.id, COALESCE(si.user_id,0), COALESCE(u.name,'公共账户'), si.item_type,
			si.ratio, si.kwh, si.revenue, si.subsidy, si.om_share, si.tax_share, si.wo_share,
			si.amount, COALESCE(si.memo,'')
		FROM settlement_items si LEFT JOIN users u ON u.id=si.user_id
		WHERE si.period_id=$1
		ORDER BY CASE si.item_type WHEN 'shareholder' THEN 1 WHEN 'public' THEN 2 ELSE 3 END, si.id`, sid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	items := []any{}
	for rows.Next() {
		var id, uid int64
		var name, itype, memo string
		var ratio, ikwh, rev, sub, omS, taxS, woS, amount float64
		if err := rows.Scan(&id, &uid, &name, &itype, &ratio, &ikwh, &rev, &sub,
			&omS, &taxS, &woS, &amount, &memo); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		items = append(items, map[string]any{
			"id": id, "user_id": uid, "name": name, "item_type": itype,
			"ratio": ratio, "kwh": ikwh, "revenue": rev, "subsidy": sub,
			"om_share": omS, "tax_share": taxS, "wo_share": woS, "amount": amount, "memo": memo,
		})
	}
	writeJSON(w, 200, map[string]any{
		"id": sid, "building_id": bid, "period_month": month, "status": status,
		"total_kwh": kwh, "grid_price": price, "subsidy_per_kwh": subRate,
		"gross_revenue": gross, "total_subsidy": totalSub, "om_fee": om, "tax": tax,
		"work_order_cost": woCost, "maintenance_fund_ratio": fundRatio,
		"maintenance_fund_used": fundUsed, "maintenance_fund_addition": fundAdd,
		"compensation": compAmt,
		"public_ratio": pubRatio, "distributable": dist, "finalized_at": finalizedAt,
		"items": items,
	})
}

// finalizeSettlement 定稿结算并入账：住户余额、公共账户、维修基金、台账流水
func (s *Server) finalizeSettlement(w http.ResponseWriter, r *http.Request) {
	u := auth.RequireRole(w, r, "admin", "property")
	if u == nil {
		return
	}
	sid := urlID(r, "sid")
	var bid int64
	var month, status string
	if err := s.Pool.QueryRow(r.Context(),
		`SELECT building_id, period_month, status FROM settlement_periods WHERE id=$1`, sid).
		Scan(&bid, &month, &status); err != nil {
		writeErr(w, 404, "结算单不存在")
		return
	}
	if status == "finalized" {
		writeErr(w, 409, "结算单已定稿")
		return
	}

	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback(r.Context())

	type itemRow struct {
		UserID  *int64
		Type    string
		Ratio   float64
		Kwh     float64
		Revenue float64
		Subsidy float64
		Om      float64
		Tax     float64
		Wo      float64
		Amount  float64
	}
	rows, err := tx.Query(r.Context(),
		`SELECT user_id,item_type,ratio,kwh,revenue,subsidy,om_share,tax_share,wo_share,amount
		 FROM settlement_items WHERE period_id=$1`, sid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var items []itemRow
	for rows.Next() {
		var it itemRow
		if err := rows.Scan(&it.UserID, &it.Type, &it.Ratio, &it.Kwh, &it.Revenue,
			&it.Subsidy, &it.Om, &it.Tax, &it.Wo, &it.Amount); err != nil {
			rows.Close()
			writeErr(w, 500, err.Error())
			return
		}
		items = append(items, it)
	}
	rows.Close()

	var grossRev, totalSub, omFee, taxAmt, woCost, fundUsed, fundAddition, compensation float64
	if err := tx.QueryRow(r.Context(),
		`SELECT gross_revenue,total_subsidy,om_fee,tax,work_order_cost,
			maintenance_fund_used,maintenance_fund_addition,compensation
		 FROM settlement_periods WHERE id=$1`,
		sid).Scan(&grossRev, &totalSub, &omFee, &taxAmt, &woCost, &fundUsed, &fundAddition, &compensation); err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	// 楼级现金账（公共账户）：收入、赔偿与费用按总额记账；
	// maintenance_fund 类别仅作专款标记，不计入公共账户现金净额。
	led := func(uid any, dir, cat string, amount float64, memo string) {
		if amount == 0 {
			return
		}
		_, _ = tx.Exec(r.Context(), `
			INSERT INTO ledger(building_id,user_id,direction,amount,category,period_month,ref_type,ref_id,memo)
			VALUES ($1,$2,$3,$4,$5,$6,'settlement',$7,$8)`,
			bid, uid, dir, amount, cat, month, sid, memo)
	}
	pay := func(ptype string, amount float64, memo string) {
		if amount == 0 {
			return
		}
		_, _ = tx.Exec(r.Context(), `
			INSERT INTO payments(building_id,user_id,amount,pay_type,memo) VALUES ($1,NULL,$2,$3,$4)`,
			bid, amount, ptype, memo)
	}
	pay("grid", grossRev, month+" 电网购电收入")
	pay("subsidy", totalSub, month+" 发电补贴")
	pay("compensation", compensation, month+" 维修责任赔偿")
	led(nil, "in", "revenue", grossRev, month+" 上网电费收入")
	led(nil, "in", "subsidy", totalSub, month+" 度电补贴")
	led(nil, "in", "compensation", compensation, month+" 安装商/保险责任赔偿")
	led(nil, "out", "om", omFee, month+" 运维费用（全额）")
	led(nil, "out", "tax", taxAmt, month+" 税费（全额）")
	led(nil, "out", "repair", woCost, month+" 维修费用（全额，含维修基金动用部分）")

	for _, it := range items {
		switch it.Type {
		case "shareholder":
			if it.UserID == nil {
				continue
			}
			uid := *it.UserID
			led(uid, "in", "revenue", it.Revenue, month+" 住户发电收益")
			led(uid, "in", "subsidy", it.Subsidy, month+" 住户补贴分成")
			led(uid, "out", "om", it.Om, month+" 运维费分摊")
			led(uid, "out", "tax", it.Tax, month+" 税费分摊")
			led(uid, "out", "repair", it.Wo, month+" 维修费分摊（基金不足部分）")
			if _, err := tx.Exec(r.Context(),
				`UPDATE users SET balance=balance+$2 WHERE id=$1`, uid, it.Amount); err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			// 为正收益住户开具月度收益凭证（净收益 ≤ 0 的月份不开票）
			if it.Amount > 0 {
				if err := issueInvoice(r.Context(), tx, bid, "normal", &uid, "",
					month+" 光伏发电收益结算凭证", it.Amount, "settlement", &sid, month, u.ID,
					"含电费收入与补贴分成，已扣除运维/税费/维修分摊"); err != nil {
					writeErr(w, 500, err.Error())
					return
				}
			}
		case "maintenance_fund":
			// 专款标记：计提/动用（不影响公共账户现金净额）
			led(nil, "in", "maintenance_fund", fundAddition, month+" 公共收益计提维修基金（专款标记）")
			led(nil, "out", "maintenance_fund", fundUsed, month+" 动用维修基金支付维修（专款标记）")
		}
	}

	if _, err := tx.Exec(r.Context(),
		`UPDATE settlement_periods SET status='finalized', finalized_at=now() WHERE id=$1`, sid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) listLedger(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	bid := urlID(r, "id")
	month := r.URL.Query().Get("period_month")
	rows, err := s.Pool.Query(r.Context(), `
		SELECT l.id, COALESCE(l.user_id,0), COALESCE(u.name,'公共账户'), l.direction, l.amount,
			l.category, COALESCE(l.period_month,''), l.memo, l.created_at
		FROM ledger l LEFT JOIN users u ON u.id=l.user_id
		WHERE l.building_id=$1 AND ($2='' OR l.period_month=$2)
		ORDER BY l.id DESC LIMIT 500`, bid, month)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var id, uid int64
		var name, dir, cat, pm, memo string
		var amount float64
		var at any
		if err := rows.Scan(&id, &uid, &name, &dir, &amount, &cat, &pm, &memo, &at); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out = append(out, map[string]any{
			"id": id, "user_id": uid, "party": name, "direction": dir,
			"amount": amount, "category": cat, "period_month": pm, "memo": memo, "created_at": at,
		})
	}
	var publicBalance, fundBalance float64
	_ = s.Pool.QueryRow(r.Context(), `
		SELECT COALESCE(SUM(CASE WHEN direction='in' THEN amount ELSE -amount END),0)
		FROM ledger WHERE building_id=$1 AND user_id IS NULL AND category<>'maintenance_fund'`, bid).Scan(&publicBalance)
	_ = s.Pool.QueryRow(r.Context(), `
		SELECT COALESCE(SUM(CASE WHEN direction='in' THEN amount ELSE -amount END),0)
		FROM ledger WHERE building_id=$1 AND category='maintenance_fund'`, bid).Scan(&fundBalance)
	writeJSON(w, 200, map[string]any{
		"items": out, "public_balance": publicBalance, "maintenance_fund_balance": fundBalance,
	})
}

func (s *Server) listPayments(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	bid := urlID(r, "id")
	rows, err := s.Pool.Query(r.Context(), `
		SELECT p.id, COALESCE(p.user_id,0), COALESCE(u.name,'公共账户'), p.amount,
			p.pay_type, p.status, COALESCE(p.memo,''), p.created_at
		FROM payments p LEFT JOIN users u ON u.id=p.user_id
		WHERE p.building_id=$1 ORDER BY p.id DESC LIMIT 300`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var id, uid int64
		var name, ptype, status, memo string
		var amount float64
		var at any
		if err := rows.Scan(&id, &uid, &name, &amount, &ptype, &status, &memo, &at); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out = append(out, map[string]any{
			"id": id, "user_id": uid, "party": name, "amount": amount,
			"pay_type": ptype, "status": status, "memo": memo, "created_at": at,
		})
	}
	writeJSON(w, 200, map[string]any{"items": out})
}

// buildingDashboard 楼栋驾驶舱：收益、支出、维修、碳减排、月度序列
func (s *Server) buildingDashboard(w http.ResponseWriter, r *http.Request) {
	if u := auth.Require(w, r); u == nil {
		return
	}
	bid := urlID(r, "id")

	d := map[string]any{}
	var stage string
	var subRatio float64
	if err := s.Pool.QueryRow(r.Context(),
		`SELECT stage, subscribed_ratio FROM buildings WHERE id=$1`, bid).Scan(&stage, &subRatio); err != nil {
		writeErr(w, 404, "楼栋不存在")
		return
	}
	d["stage"] = stage
	d["subscribed_ratio"] = subRatio

	var totalKwh, grossRevenue, totalSubsidy, distributed, omFee, taxAmt, repairOut, compensation float64
	_ = s.Pool.QueryRow(r.Context(), `
		SELECT
			COALESCE(SUM(sp.total_kwh),0),
			COALESCE(SUM(sp.gross_revenue) FILTER (WHERE sp.status='finalized'),0),
			COALESCE(SUM(sp.total_subsidy) FILTER (WHERE sp.status='finalized'),0),
			COALESCE(SUM(sp.distributable) FILTER (WHERE sp.status='finalized'),0),
			COALESCE(SUM(sp.om_fee) FILTER (WHERE sp.status='finalized'),0),
			COALESCE(SUM(sp.tax) FILTER (WHERE sp.status='finalized'),0),
			COALESCE(SUM(sp.work_order_cost) FILTER (WHERE sp.status='finalized'),0),
			0
		FROM settlement_periods sp WHERE sp.building_id=$1`,
		bid).Scan(&totalKwh, &grossRevenue, &totalSubsidy, &distributed, &omFee, &taxAmt, &repairOut, &compensation)
	_ = s.Pool.QueryRow(r.Context(),
		`SELECT COALESCE(SUM(amount),0) FROM payments WHERE building_id=$1 AND pay_type='compensation'`,
		bid).Scan(&compensation)

	var publicBalance, fundBalance float64
	_ = s.Pool.QueryRow(r.Context(), `
		SELECT COALESCE(SUM(CASE WHEN direction='in' THEN amount ELSE -amount END),0)
		FROM ledger WHERE building_id=$1 AND user_id IS NULL AND category<>'maintenance_fund'`, bid).Scan(&publicBalance)
	_ = s.Pool.QueryRow(r.Context(), `
		SELECT COALESCE(SUM(CASE WHEN direction='in' THEN amount ELSE -amount END),0)
		FROM ledger WHERE building_id=$1 AND category='maintenance_fund'`, bid).Scan(&fundBalance)

	var residentCount, woOpen, woResolved int
	_ = s.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM building_members WHERE building_id=$1 AND member_type='resident'`, bid).Scan(&residentCount)
	_ = s.Pool.QueryRow(r.Context(),
		`SELECT count(*) FILTER (WHERE status<>'resolved'), count(*) FILTER (WHERE status='resolved')
		 FROM work_orders WHERE building_id=$1`, bid).Scan(&woOpen, &woResolved)

	rows, err := s.Pool.Query(r.Context(), `
		SELECT period_month, total_kwh, gross_revenue, total_subsidy, distributable, status
		FROM settlement_periods WHERE building_id=$1 ORDER BY period_month`, bid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	series := []any{}
	for rows.Next() {
		var month, st string
		var kwh, rev, sub, dist float64
		if err := rows.Scan(&month, &kwh, &rev, &sub, &dist, &st); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		series = append(series, map[string]any{
			"period_month": month, "kwh": kwh, "gross_revenue": rev,
			"subsidy": sub, "distributable": dist, "status": st,
		})
	}

	d["total_kwh"] = totalKwh
	d["carbon_kg"] = totalKwh * settlement.CarbonFactorKgPerKwh
	d["gross_revenue"] = grossRevenue
	d["total_subsidy"] = totalSubsidy
	d["distributable"] = distributed
	d["om_fee"] = omFee
	d["tax"] = taxAmt
	d["work_order_cost"] = repairOut
	d["compensation"] = compensation
	d["public_balance"] = publicBalance
	d["maintenance_fund_balance"] = fundBalance
	d["resident_count"] = residentCount
	d["work_orders_open"] = woOpen
	d["work_orders_resolved"] = woResolved
	d["monthly_series"] = series
	writeJSON(w, 200, d)
}
