#!/usr/bin/env python3
"""端到端 API 验证脚本（依赖 python3，无第三方包）。

用法：
    python3 scripts/verify_api.py [BASE_URL]

前置：服务已启动且数据库为初始种子状态（SEED_ON_START=true 首次启动后的干净库）。
脚本会校验：驾驶舱 → 9 月结算（维修基金动用+分摊）→ 定稿入账 → 转让前检查 →
认购权益随房转让（买方接受→物业审核→发票换开）→ 交割附件导出 →
次月分成归属切换 → 公告/异议/异常解释/年报 → 阶段回退原因强制。
任一断言失败立即退出（exit 1）。
"""
import json
import sys
import urllib.request
import urllib.error

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8080"


def call(method, path, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req) as r:
            raw = r.read().decode()
            return r.status, json.loads(raw) if raw else {}
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, {"raw": raw}


def call_raw(path, token):
    req = urllib.request.Request(BASE + path, method="GET")
    req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req) as r:
            return r.status, r.read(), r.headers.get("Content-Type", "")
    except urllib.error.HTTPError as e:
        return e.code, e.read(), ""


def check(name, cond, detail=""):
    if not cond:
        print(f"FAIL: {name} {detail}")
        sys.exit(1)
    print(f"PASS: {name} {detail}")


def approx(a, b, eps=0.01):
    return abs(float(a) - float(b)) < eps


def login(phone, pwd):
    st, r = call("POST", "/api/auth/login", body={"phone": phone, "password": pwd})
    check(f"login {phone}", st == 200)
    return r["token"]


def main():
    prop = login("13800000001", "property123")
    zhang = login("13900000001", "resident123")
    liu = login("13900000006", "resident123")

    st, r = call("GET", "/api/buildings", prop)
    b1 = next(b for b in r["items"] if "1 号楼" in b["name"])
    bid = b1["id"]
    print(f"-- 目标楼栋：{b1['name']} (id={bid})，阶段 {b1['stage']}")

    # A. 8 月已定稿的驾驶舱
    st, d = call("GET", f"/api/buildings/{bid}/dashboard", prop)
    check("8月后公共账户余额=85631.00", approx(d["public_balance"], 85631.00), str(d["public_balance"]))
    check("维修基金余额=97.86", approx(d["maintenance_fund_balance"], 97.86), str(d["maintenance_fund_balance"]))
    check("8月累计发电4150", approx(d["total_kwh"], 4150))
    check("碳减排2411.15kg", approx(d["carbon_kg"], 2411.15))

    # B. 9 月试算：2600kWh，漏水 600 物业责任，基金期初 97.86
    st, r = call("POST", f"/api/buildings/{bid}/settlements", prop, {
        "period_month": "2026-09", "grid_price": 0.4153, "subsidy_per_kwh": 0.05,
        "om_fee": 200, "tax": 120,
    })
    s = r["settlement"]
    check("9月动用基金97.86", approx(s["fund_used"], 97.86), str(s["fund_used"]))
    check("9月维修分摊502.14", approx(s["repair_deduct"], 502.14), str(s["repair_deduct"]))
    check("9月计提基金23.26", approx(s["fund_addition"], 23.26), str(s["fund_addition"]))
    zhang_item = next(i for i in s["items"] if i["item_type"] == "shareholder" and i["name"].startswith("张伟"))
    check("张伟9月分成116.29", approx(zhang_item["amount"], 116.29), str(zhang_item["amount"]))
    spid = r["id"]

    # C. 定稿（张伟 8 月 489.30 − 种子提现 200 + 9 月 116.29 = 405.59）
    st, _ = call("POST", f"/api/settlements/{spid}/finalize", prop)
    check("9月结算定稿", st == 200)
    st, _ = call("POST", f"/api/buildings/{bid}/settlements", prop,
                 {"period_month": "2026-09", "grid_price": 0.5})
    check("定稿后禁止重算(409)", st == 409, str(st))
    st, me = call("GET", "/api/me", zhang)
    check("张伟余额=489.30-200+116.29=405.59", approx(me["balance"], 405.59), str(me["balance"]))
    st, lg = call("GET", f"/api/buildings/{bid}/ledger", prop)
    check("9月后公共账户=85920.78", approx(lg["public_balance"], 85920.78), str(lg["public_balance"]))
    check("9月后维修基金=23.26", approx(lg["maintenance_fund_balance"], 23.26), str(lg["maintenance_fund_balance"]))

    # C2. 种子提现记录（张伟 200.00）
    st, wd = call("GET", "/api/me/withdrawals", zhang)
    check("张伟有1笔种子提现200.00",
          st == 200 and len(wd["items"]) == 1 and approx(wd["items"][0]["amount"], 200.00), str(wd)[:120])

    # D. 转让前检查：份额/未结收益/维修分摊/三种方式影响
    st, subs = call("GET", f"/api/buildings/{bid}/subscriptions", prop)
    sid = next(x["id"] for x in subs["items"] if x["name"].startswith("张伟") and x["type"] == "shareholder")
    st, ck = call("GET", f"/api/subscriptions/{sid}/transfer-check", zhang)
    check("转让前检查可办理", st == 200 and ck["can_transfer"] is True, str(ck.get("blockers")))
    check("检查：认购份额30%", approx(ck["subscription"]["ratio"], 30), str(ck["subscription"]))
    check("检查：未结收益0期", ck["unsettled"]["draft_count"] == 0, str(ck["unsettled"]))
    check("检查：维修分摊敞口0单", ck["maintenance"]["open_count"] == 0, str(ck["maintenance"]))
    check("检查：随房转让比例不变70%", approx(ck["modes"]["with_house"]["ratio_after"], 70))
    check("检查：提前退出后比例40%", approx(ck["modes"]["early_settlement"]["ratio_after"], 40))

    # E. 认购权益随房转让：张伟 -> 刘洋（买方接受 → 物业审核 → 发票换开）
    st, users = call("GET", "/api/users?q=13900000006", prop)
    liu_id = next(u["id"] for u in users["items"] if u["phone"] == "13900000006")
    st, tr = call("POST", f"/api/buildings/{bid}/transfers", zhang, {
        "subscription_id": sid, "mode": "with_house", "to_user_id": liu_id,
        "materials": ["房屋买卖合同", "不动产权证（或过户受理单）复印件", "光伏认购权益受让确认书"],
    })
    check("发起随房转让", st == 201 and tr["history_settled"] is True, str(tr)[:200])
    tid = tr["id"]
    st, _ = call("POST", f"/api/transfers/{tid}/confirm", prop)
    check("买方未接受时确认被拒(409)", st == 409, str(st))
    st, _ = call("POST", f"/api/transfers/{tid}/accept", liu, {"accept": True})
    check("买方刘洋在线接受权益", st == 200, str(st))
    st, cf = call("POST", f"/api/transfers/{tid}/confirm", prop,
                  {"review_note": "过户材料齐全，审核通过"})
    check("物业审核办理", st == 200 and cf.get("invoice_no"), str(cf))
    st, b = call("GET", f"/api/buildings/{bid}", prop)
    check("随房转让后楼栋认购比例仍70%", approx(b["subscribed_ratio"], 70), str(b["subscribed_ratio"]))
    st, invs = call("GET", f"/api/buildings/{bid}/invoices", prop)
    old_inv = [v for v in invs["items"] if v["subscription_id"] == sid and v["inv_type"] == "subscription"]
    reissue = [v for v in invs["items"] if v["inv_type"] == "reissue" and v["user_id"] == liu_id]
    check("原认购发票已换开作废", len(old_inv) == 1 and old_inv[0]["status"] == "replaced", str(old_inv))
    check("换开发票抬头刘洋36000",
          len(reissue) == 1 and reissue[0]["status"] == "valid" and approx(reissue[0]["amount"], 36000),
          str(reissue))

    # E2. 争议登记 + 交割附件导出（月度发电/结算/维修扣费/已提现）
    st, _ = call("POST", f"/api/transfers/{tid}/dispute", liu,
                 {"note": "买方要求留存 9 月维修分摊明细"})
    check("登记历史收益争议", st == 200, str(st))
    st, ex = call("GET", f"/api/transfers/{tid}/export", zhang)
    check("导出：月度发电4条", st == 200 and len(ex["generation"]) == 4, str(len(ex.get("generation", []))))
    check("导出：张伟结算2期(8/9月)", len(ex["settlements"]) == 2, str(len(ex.get("settlements", []))))
    check("导出：维修扣费1期(9月502.14的30%)",
          len(ex["maintenance"]) == 1 and approx(ex["maintenance"][0]["wo_share"], 150.64),
          str(ex.get("maintenance")))
    check("导出：已提现1笔200", len(ex["withdrawals"]) == 1 and approx(ex["withdrawals"][0]["amount"], 200))
    check("导出：发票2张(原票+换开)", len(ex["invoices"]) == 2, str(len(ex.get("invoices", []))))
    check("导出：含争议记录", "维修分摊" in ex["dispute_note"], ex["dispute_note"][:60])
    code, body, ct = call_raw(f"/api/transfers/{tid}/export?format=csv", liu)
    check("导出CSV含关键章节", code == 200 and "月度发电".encode() in body and "已提现".encode() in body,
          f"{code} {ct}")
    st, _ = call("GET", f"/api/transfers/{tid}/export", login("13900000004", "resident123"))
    check("无关住户导出被拒(403)", st == 403, str(st))

    # F. 10 月发电与结算：30% 份额应归刘洋（审核通过后下一期按新权益人发放）
    st, invs2 = call("GET", f"/api/buildings/{bid}/inverters", prop)
    for iv in invs2["items"]:
        st, _ = call("POST", f"/api/buildings/{bid}/generation", prop, {
            "period_month": "2026-10", "inverter_id": iv["id"], "kwh": 1200,
        })
        check(f"录入{iv['name']}10月电量", st == 200)
    st, r = call("POST", f"/api/buildings/{bid}/settlements", prop, {
        "period_month": "2026-10", "grid_price": 0.4153, "subsidy_per_kwh": 0.05,
    })
    check("10月结算生成", st == 200, str(r)[:200])
    octid = r["id"]
    st, detail = call("GET", f"/api/settlements/{octid}", prop)
    holders = {i["name"]: i["amount"] for i in detail["items"] if i["item_type"] == "shareholder"}
    check("张伟已无10月分成", not any(k.startswith("张伟") for k in holders), str(holders))
    check("刘洋(新业主)获30%分成335.02", approx(holders.get("刘洋(新业主)", 0), 335.02), str(holders))
    call("POST", f"/api/settlements/{octid}/finalize", prop)
    st, me = call("GET", "/api/me", liu)
    check("刘洋余额=335.02", approx(me["balance"], 335.02), str(me["balance"]))
    st, me = call("GET", "/api/me", zhang)
    check("张伟余额保持405.59（历史收益不转移）", approx(me["balance"], 405.59), str(me["balance"]))

    # G. 公告 / 异议 / 异常解释 / 年报
    st, _ = call("POST", f"/api/buildings/{bid}/announcements", prop,
                 {"title": "10月公示", "content": "见结算", "pinned": True})
    check("发布公告", st == 201)
    st, poll = call("POST", f"/api/buildings/{bid}/polls", zhang,
                    {"title": "验证用异议", "content": "测试"})
    pid = poll["id"]
    call("POST", f"/api/polls/{pid}/vote", zhang, {"support": True})
    call("POST", f"/api/polls/{pid}/vote", liu, {"support": True})
    st, r = call("POST", f"/api/polls/{pid}/close", prop)
    check("异议投票成立", r["result"] == "approved", str(r))
    st, al = call("POST", f"/api/buildings/{bid}/alerts", zhang,
                  {"period_month": "2026-10", "reason": "脚本验证异常"})
    aid = al["id"]
    st, _ = call("POST", f"/api/alerts/{aid}/explain", prop, {"explanation": "脚本验证：计划检修 1 天"})
    check("异常解释", st == 200)
    st, r = call("POST", f"/api/buildings/{bid}/annual-reports", prop, {"year": 2026})
    check("年度报告生成（3个定稿月份）", st == 200 and r["summary"]["finalized_months"] == 3, str(st))
    rid = r["id"]
    st, _ = call("POST", f"/api/annual-reports/{rid}/publish", prop)
    check("年报发布", st == 200)

    # H. 阶段回退必须有原因
    st, _ = call("POST", f"/api/buildings/{bid}/stage", prop, {"to_stage": "construction"})
    check("无原因回退被拒(409)", st == 409, str(st))
    st, r = call("POST", f"/api/buildings/{bid}/stage", prop,
                 {"to_stage": "construction", "reason": "设备供应延迟，组件到货推迟"})
    check("有原因回退成功", st == 200, str(r))

    print("\n全部端到端断言通过 ✅")


if __name__ == "__main__":
    main()
