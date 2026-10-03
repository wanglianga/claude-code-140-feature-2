#!/usr/bin/env python3
"""端到端 API 验证脚本（依赖 python3，无第三方包）。

用法：
    python3 scripts/verify_api.py [BASE_URL]

前置：服务已启动且数据库为初始种子状态（SEED_ON_START=true 首次启动后的干净库）。
脚本会校验：驾驶舱 → 9 月结算（维修基金动用+分摊）→ 定稿入账 →
平台核查 → 买方接受 → 物业材料审核随房转让 → 次月分成归属切换 →
收益提现放款 → 票据（转让凭证/作废冲红）→ 交割附件 CSV →
提前结算退出（退款+冲红+认购比例下调）→ 同楼栋转让次月分成 →
公告/异议/异常解释/年报 → 草稿期转让阻断 → 阶段回退原因强制。
任一断言失败立即退出（exit 1）。
"""
import json
import sys
import urllib.request
import urllib.error

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8080"


def call(method, path, token=None, body=None, raw=False):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req) as r:
            text = r.read().decode()
            if raw:
                return r.status, text
            return r.status, json.loads(text) if text else {}
    except urllib.error.HTTPError as e:
        text = e.read().decode()
        if raw:
            return e.code, text
        try:
            return e.code, json.loads(text)
        except Exception:
            return e.code, {"raw": text}


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
    wang = login("13900000003", "resident123")
    chen = login("13900000005", "resident123")

    st, r = call("GET", "/api/buildings", prop)
    b1 = next(b for b in r["items"] if "1 号楼" in b["name"])
    bid = b1["id"]
    print(f"-- 目标楼栋：{b1['name']} (id={bid})，阶段 {b1['stage']}")

    # A. 8 月已定稿的驾驶舱（张伟已提现 200 元只影响个人账户，不影响公共账户）
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

    # C. 定稿
    st, _ = call("POST", f"/api/settlements/{spid}/finalize", prop)
    check("9月结算定稿", st == 200)
    st, _ = call("POST", f"/api/buildings/{bid}/settlements", prop,
                 {"period_month": "2026-09", "grid_price": 0.5})
    check("定稿后禁止重算(409)", st == 409, str(st))
    st, me = call("GET", "/api/me", zhang)
    check("张伟余额=8月489.30+9月116.29-已提现200=405.59", approx(me["balance"], 405.59), str(me["balance"]))
    st, lg = call("GET", f"/api/buildings/{bid}/ledger", prop)
    check("9月后公共账户=85920.78", approx(lg["public_balance"], 85920.78), str(lg["public_balance"]))
    check("9月后维修基金=23.26", approx(lg["maintenance_fund_balance"], 23.26), str(lg["maintenance_fund_balance"]))

    # D. 平台核查 → 买方接受 → 物业审核材料：张伟 -> 刘洋（随房转让）
    st, subs = call("GET", f"/api/buildings/{bid}/subscriptions", prop)
    sid = next(x["id"] for x in subs["items"] if x["name"].startswith("张伟") and x["type"] == "shareholder")
    wang_sid = next(x["id"] for x in subs["items"] if x["name"].startswith("王强") and x["type"] == "shareholder")
    st, users = call("GET", "/api/users?q=13900000006", prop)
    liu_id = next(u["id"] for u in users["items"] if u["phone"] == "13900000006")

    st, chk = call("GET",
                   f"/api/buildings/{bid}/transfer-check?subscription_id={sid}&to_user_id={liu_id}", zhang)
    check("核查接口200", st == 200, str(st))
    check("核查认购份额30%", approx(chk["panel_ratio"], 30), str(chk["panel_ratio"]))
    check("核查收益账户余额405.59", approx(chk["balance"], 405.59), str(chk["balance"]))
    check("核查历史维修分摊>0", float(chk["maintenance_share_finalized"]) > 0, str(chk["maintenance_share_finalized"]))
    check("买方初始未接受", chk["buyer"]["accepted"] is False)
    check("权益生效月=2026-10", chk["effective_month"] == "2026-10", str(chk["effective_month"]))
    check("阻断项提示买方未接受", any("买方尚未" in b for b in chk["blockers"]), str(chk["blockers"]))

    st, tr = call("POST", f"/api/buildings/{bid}/transfers", zhang, {
        "subscription_id": sid, "deal_type": "follow_house", "to_user_id": liu_id,
    })
    check("发起随房转让", st == 201 and tr["history_settled"] is True, str(tr))
    tid = tr["id"]
    st, _ = call("POST", f"/api/transfers/{tid}/accept", zhang)
    check("非买方不能代为接受(403)", st == 403, str(st))
    st, _ = call("POST", f"/api/transfers/{tid}/accept", liu)
    check("买方刘洋确认接受", st == 200, str(st))
    st, chk = call("GET",
                   f"/api/buildings/{bid}/transfer-check?subscription_id={sid}&to_user_id={liu_id}", zhang)
    check("买方接受后核查无阻断项", st == 200 and len(chk["blockers"]) == 0, str(chk.get("blockers")))
    st, _ = call("POST", f"/api/transfers/{tid}/confirm", prop, {})
    check("未登记过户材料被拒(400)", st == 400, str(st))
    st, r = call("POST", f"/api/transfers/{tid}/confirm", prop, {
        "documents": "不动产权证/房屋买卖合同、买卖双方身份证明、光伏认购权益转让协议",
        "review_memo": "材料齐全，同意过户",
    })
    check("物业审核材料并确认", st == 200 and r["effective_month"] == "2026-10", str(r))

    # E. 10 月发电与结算：30% 份额应归刘洋
    st, invs = call("GET", f"/api/buildings/{bid}/inverters", prop)
    for iv in invs["items"]:
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

    # F. 公告 / 异议 / 异常解释 / 年报（3 个定稿月份）
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

    # G. 收益提现：刘洋提现 100，物业放款 → 余额 235.02
    st, wds = call("GET", f"/api/buildings/{bid}/withdrawals", liu)
    check("刘洋可提余额335.02", approx(wds["available"]["me"], 335.02), str(wds["available"]))
    st, wd = call("POST", f"/api/buildings/{bid}/withdrawals", liu,
                  {"amount": 100, "bank_info": "建设银行 6217…4021"})
    check("提现申请成功", st == 201, str(wd))
    wid = wd["id"]
    st, _ = call("POST", f"/api/buildings/{bid}/withdrawals", liu,
                 {"amount": 400, "bank_info": "x"})
    check("超额提现被拒(409)", st == 409, str(st))
    st, _ = call("POST", f"/api/withdrawals/{wid}/review", prop, {"approve": True})
    check("物业放款", st == 200)
    st, me = call("GET", "/api/me", liu)
    check("刘洋放款后余额235.02", approx(me["balance"], 235.02), str(me["balance"]))

    # H. 票据：权益转让凭证存在；张伟认购票可作废并补开红字凭证
    st, invs = call("GET", f"/api/buildings/{bid}/invoices", prop)
    transfer_inv = [i for i in invs["items"]
                    if i["ref_type"] == "transfer" and i["buyer_name"].startswith("刘洋")]
    check("存在刘洋名下权益转让凭证", len(transfer_inv) >= 1, str(len(transfer_inv)))
    zhang_sub_inv = next((i for i in invs["items"]
                          if i["ref_type"] == "subscription" and i["buyer_name"].startswith("张伟")
                          and i["status"] == "issued"), None)
    check("张伟存在有效认购票据", zhang_sub_inv is not None)
    st, r = call("POST", f"/api/invoices/{zhang_sub_inv['id']}/void", prop)
    check("作废票据并补开红字凭证", st == 200 and bool(r.get("red_invoice_no")), str(r))
    st, invs = call("GET", f"/api/buildings/{bid}/invoices", prop)
    check("红字冲销凭证可查", any(i["invoice_type"] == "red" and i["buyer_name"].startswith("张伟")
                                 for i in invs["items"]))

    # I. 交割附件 CSV：含月度发电、结算、维修扣费、已提现、票据
    st, csv_text = call("GET",
                        f"/api/buildings/{bid}/transfer-export?subscription_id={sid}", liu, raw=True)
    check("交割附件导出200", st == 200, str(st))
    for section in ["月度发电量", "月度结算明细", "维修扣费", "已提现记录", "票据记录"]:
        check(f"CSV含「{section}」段", section in csv_text)
    check("CSV含9月漏水扣费月份2026-09", "2026-09" in csv_text)
    check("CSV含历任权益人（张伟/刘洋）", "张伟" in csv_text and "刘洋" in csv_text)

    # J. 王强提前结算退出：退认购款 + 冲红票据 + 楼栋认购比例 70%→55%
    st, tr = call("POST", f"/api/buildings/{bid}/transfers", wang,
                  {"subscription_id": wang_sid, "deal_type": "early_exit"})
    check("发起提前结算退出", st == 201, str(tr))
    exit_id = tr["id"]
    st, r = call("POST", f"/api/transfers/{exit_id}/confirm", prop, {
        "documents": "房屋买卖合同、提前结算退出确认书", "review_memo": "同意退出",
    })
    check("物业确认提前退出", st == 200, str(r))
    st, subs = call("GET", f"/api/buildings/{bid}/subscriptions", prop)
    check("退出后楼栋认购比例=55%（刘洋30+李娜25）", approx(subs["subscribed_ratio"], 55),
          str(subs["subscribed_ratio"]))
    st, invs = call("GET", f"/api/buildings/{bid}/invoices", prop)
    check("王强名下存在红字冲销票", any(i["invoice_type"] == "red" and i["buyer_name"].startswith("王强")
                                        for i in invs["items"]))
    st, pays = call("GET", f"/api/buildings/{bid}/payments", prop)
    check("存在退出退款流水", any(p["pay_type"] == "refund" and p["party"].startswith("王强")
                                 for p in pays["items"]))

    # K. 李娜把 25% 转给同楼栋住户陈杰（成员），次月分成归陈杰
    lina = login("13900000002", "resident123")
    lina_sid = next(x["id"] for x in subs["items"] if x["name"].startswith("李娜") and x["type"] == "shareholder")
    st, users = call("GET", "/api/users?q=13900000005", prop)
    chen_id = next(u["id"] for u in users["items"] if u["phone"] == "13900000005")
    st, tr = call("POST", f"/api/buildings/{bid}/transfers", lina, {
        "subscription_id": lina_sid, "deal_type": "transfer_neighbor", "to_user_id": chen_id,
    })
    check("发起同楼栋转让", st == 201, str(tr))
    nb_id = tr["id"]
    st, _ = call("POST", f"/api/transfers/{nb_id}/accept", chen)
    check("陈杰接受权益", st == 200, str(st))
    st, r = call("POST", f"/api/transfers/{nb_id}/confirm", prop,
                 {"documents": "权益转让协议、双方身份证明"})
    check("同楼栋转让审核通过、生效月2026-11", st == 200 and r["effective_month"] == "2026-11", str(r))
    for iv in invs["items"]:
        pass
    st, invlist = call("GET", f"/api/buildings/{bid}/inverters", prop)
    for iv in invlist["items"]:
        call("POST", f"/api/buildings/{bid}/generation", prop, {
            "period_month": "2026-11", "inverter_id": iv["id"], "kwh": 1200,
        })
    st, r = call("POST", f"/api/buildings/{bid}/settlements", prop, {
        "period_month": "2026-11", "grid_price": 0.4153, "subsidy_per_kwh": 0.05,
    })
    check("11月结算生成", st == 200, str(r)[:200])
    novid = r["id"]
    st, detail = call("GET", f"/api/settlements/{novid}", prop)
    nov = {i["name"]: i["amount"] for i in detail["items"] if i["item_type"] == "shareholder"}
    check("陈杰获25%分成279.18", approx(nov.get("陈杰", 0), 279.18), str(nov))
    check("李娜无11月分成", not any(k.startswith("李娜") for k in nov), str(nov))
    call("POST", f"/api/settlements/{novid}/finalize", prop)

    # L. 草稿期转让阻断：12 月先生成草稿，确认转让必须被拒
    st, invlist = call("GET", f"/api/buildings/{bid}/inverters", prop)
    for iv in invlist["items"]:
        call("POST", f"/api/buildings/{bid}/generation", prop, {
            "period_month": "2026-12", "inverter_id": iv["id"], "kwh": 800,
        })
    st, r = call("POST", f"/api/buildings/{bid}/settlements", prop,
                 {"period_month": "2026-12", "grid_price": 0.4})
    check("12月草稿生成", st == 200, str(r)[:200])
    # 陈杰尝试再次转出给刘洋
    st, tr = call("POST", f"/api/buildings/{bid}/transfers", chen, {
        "subscription_id": lina_sid, "deal_type": "transfer_neighbor", "to_user_id": liu_id,
    })
    check("草稿期仍可发起申请", st == 201, str(tr))
    block_id = tr["id"]
    st, _ = call("POST", f"/api/transfers/{block_id}/accept", liu)
    st, r = call("POST", f"/api/transfers/{block_id}/confirm", prop, {"documents": "协议"})
    check("草稿期物业确认被阻断(409)", st == 409 and "未定稿" in r["error"], str(r))

    # M. 阶段回退必须有原因
    st, _ = call("POST", f"/api/buildings/{bid}/stage", prop, {"to_stage": "construction"})
    check("无原因回退被拒(409)", st == 409, str(st))
    st, r = call("POST", f"/api/buildings/{bid}/stage", prop,
                 {"to_stage": "construction", "reason": "设备供应延迟，组件到货推迟"})
    check("有原因回退成功", st == 200, str(r))

    print("\n全部端到端断言通过 ✅")


if __name__ == "__main__":
    main()
