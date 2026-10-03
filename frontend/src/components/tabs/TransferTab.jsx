import React, { useEffect, useMemo, useState } from 'react'
import { api, apiDownload, fmtMoney, DEAL_TYPE_LABEL } from '../../lib/api'
import { Badge, handleErr, useToast } from '../ui'

const DOC_PRESET = '不动产权证/房屋买卖合同、买卖双方身份证明、光伏认购权益转让协议'

export default function TransferTab({ user, building }) {
  const { notify } = useToast()
  const isManager = user.role === 'admin' || user.role === 'property'
  const [subs, setSubs] = useState([])
  const [transfers, setTransfers] = useState([])
  const [users, setUsers] = useState([])
  const [members, setMembers] = useState([])
  const [withdrawals, setWithdrawals] = useState({ items: [], available: {} })
  const [invoices, setInvoices] = useState([])
  const [checkSid, setCheckSid] = useState(0)
  const [checkBuyer, setCheckBuyer] = useState(0)
  const [check, setCheck] = useState(null)
  const [form, setForm] = useState({
    subscription_id: 0, deal_type: 'follow_house', to_user_id: 0, memo: '',
  })
  const [wdForm, setWdForm] = useState({ amount: '', bank_info: '' })

  const load = async () => {
    const [s, t, u, m, wd, inv] = await Promise.all([
      api('GET', `/buildings/${building.id}/subscriptions`),
      api('GET', `/buildings/${building.id}/transfers`),
      isManager ? api('GET', '/users') : Promise.resolve({ items: [] }),
      api('GET', `/buildings/${building.id}/members`),
      api('GET', `/buildings/${building.id}/withdrawals`),
      api('GET', `/buildings/${building.id}/invoices`),
    ])
    setSubs(s.items || [])
    setTransfers(t.items || [])
    setUsers(u.items || [])
    setMembers(m.items || [])
    setWithdrawals(wd)
    setInvoices(inv.items || [])
  }
  useEffect(() => { load() }, [building.id])

  const memberIDs = useMemo(() => new Set(members.map((m) => m.id)), [members])
  const paidSubs = subs.filter((x) => x.status === 'paid' && x.type === 'shareholder')
  // 买方候选：管理端可取全部已注册住户；普通住户只能选本楼栋成员（外部买方可先注册并请物业登记为本楼栋成员）
  const residentUsers = users.filter((u) => u.role === 'resident' && u.id !== user.id)
  const buyerCandidates = isManager && residentUsers.length > 0
    ? residentUsers
    : members.filter((m) => (m.member_type === 'resident' || m.role === 'resident') && m.id !== user.id)

  const runCheck = async () => {
    if (!checkSid) return notify('请先选择认购记录', true)
    try {
      const qs = checkBuyer ? `&to_user_id=${checkBuyer}` : ''
      const r = await handleErr(
        api('GET', `/buildings/${building.id}/transfer-check?subscription_id=${checkSid}${qs}`),
        notify,
      )
      setCheck(r)
    } catch { /* toast */ }
  }

  const submit = async () => {
    if (!form.subscription_id) return notify('请选择要办理的认购记录', true)
    if (form.deal_type !== 'early_exit' && !form.to_user_id) {
      return notify('请选择已注册的受让方住户（由买方登录确认接受权益）', true)
    }
    try {
      await handleErr(api('POST', `/buildings/${building.id}/transfers`, form), notify)
      notify('申请已提交：等待买方接受（如适用）与物业审核过户材料')
      setForm({ subscription_id: 0, deal_type: 'follow_house', to_user_id: 0, memo: '' })
      setCheck(null)
      load()
    } catch { /* toast */ }
  }

  const accept = async (t) => {
    if (!window.confirm('确认接受该房屋的光伏认购权益？确认后物业将审核过户材料。')) return
    try {
      await handleErr(api('POST', `/transfers/${t.id}/accept`), notify)
      notify('已确认接受权益，等待物业审核材料')
      load()
    } catch { /* toast */ }
  }

  const confirm = async (t) => {
    const docs = window.prompt('请登记已核验的过户材料（必填，多项用顿号分隔）：', DOC_PRESET)
    if (docs === null) return
    if (!docs.trim()) return notify('过户材料为必填项', true)
    const memo = window.prompt('审核意见（可留空）：', '') || ''
    try {
      const r = await handleErr(api('POST', `/transfers/${t.id}/confirm`, {
        documents: docs, review_memo: memo,
      }), notify)
      notify(r.note || '办理完成')
      load()
    } catch { /* toast */ }
  }
  const reject = async (t) => {
    const memo = window.prompt('驳回原因：', '')
    if (memo === null) return
    try {
      await handleErr(api('POST', `/transfers/${t.id}/reject`, { memo }), notify)
      load()
    } catch { /* toast */ }
  }

  const exportCSV = async (sid, unit) => {
    try {
      await apiDownload(`/buildings/${building.id}/transfer-export?subscription_id=${sid}`,
        `交割附件_${unit || sid}.csv`)
      notify('交割附件（CSV）已导出')
    } catch (e) { notify(e.message, true) }
  }

  const submitWithdrawal = async () => {
    const amount = Number(wdForm.amount)
    if (!(amount > 0)) return notify('请输入大于 0 的提现金额', true)
    if (!wdForm.bank_info.trim()) return notify('请填写收款账户信息', true)
    try {
      await handleErr(api('POST', `/buildings/${building.id}/withdrawals`, wdForm), notify)
      notify('提现申请已提交，等待物业放款审核')
      setWdForm({ amount: '', bank_info: '' })
      load()
    } catch { /* toast */ }
  }
  const reviewWD = async (w, approve) => {
    let memo = ''
    if (!approve) {
      memo = window.prompt('驳回原因：', '')
      if (memo === null) return
    }
    try {
      await handleErr(api('POST', `/withdrawals/${w.id}/review`, { approve, memo }), notify)
      notify(approve ? '已放款并扣减收益账户' : '已驳回，额度退回')
      load()
    } catch { /* toast */ }
  }
  const voidInvoice = async (inv) => {
    if (!window.confirm(`确认作废票据 ${inv.invoice_no} 并补开红字冲销凭证？`)) return
    try {
      await handleErr(api('POST', `/invoices/${inv.id}/void`), notify)
      notify('已作废并开具红字凭证')
      load()
    } catch { /* toast */ }
  }

  const myAvailable = Number(withdrawals.available?.me || 0)

  return (
    <>
      {/* 平台核查 */}
      <div className="card">
        <h2>过户前平台核查</h2>
        <div className="muted-box" style={{ marginBottom: 12 }}>
          住户出售房屋时，平台核查该户<b>光伏认购份额</b>、<b>未结收益</b>（收益账户余额与草稿期未入账金额）、
          <b>维修分摊</b>（历史已定稿分摊与未结工单）以及<b>买方是否接受权益</b>；
          存在未定稿月度结算或买方未接受时，物业无法办理。
        </div>
        <div className="form-row">
          <label className="field">
            <span>认购记录</span>
            <select value={checkSid} onChange={(e) => { setCheckSid(Number(e.target.value)); setCheck(null) }}>
              <option value={0}>请选择…</option>
              {paidSubs.map((s) => (
                <option key={s.id} value={s.id}>{s.name}（{s.unit_no}）· {Number(s.ratio).toFixed(1)}%</option>
              ))}
            </select>
          </label>
          <label className="field">
            <span>意向买方（用于核查接受状态）</span>
            <select value={checkBuyer} onChange={(e) => { setCheckBuyer(Number(e.target.value)); setCheck(null) }}>
              <option value={0}>不指定…</option>
              {buyerCandidates.map((u) => (
                <option key={u.id} value={u.id}>{u.name}（{u.unit_no || u.phone}）</option>
              ))}
            </select>
          </label>
          <div className="field"><span>&nbsp;</span><button className="primary" onClick={runCheck}>立即核查</button></div>
        </div>
        {check && (
          <table style={{ marginTop: 8 }}>
            <tbody>
              <tr><td>认购份额</td><td>{Number(check.panel_ratio).toFixed(2)}%（{check.panel_count} 块，认购款 ¥{fmtMoney(check.subscription_amount)}）</td>
                <td>收益账户余额</td><td>¥{fmtMoney(check.balance)}（历史已结算、可提现）</td></tr>
              <tr><td>草稿期未入账收益</td><td>¥{fmtMoney(check.draft_unposted)}
                  {check.draft_periods?.length > 0 && <span className="small muted">（{check.draft_periods.map((d) => d.period_month).join('、')} 未定稿）</span>}</td>
                <td>历史维修分摊合计</td><td>¥{fmtMoney(check.maintenance_share_finalized)}</td></tr>
              <tr><td>未结维修工单</td><td>{check.maintenance_pending_wo} 个</td>
                <td>最近定稿月份 / 权益生效月</td><td>{check.last_finalized_month || '—'} → <b>{check.effective_month || '—'}</b></td></tr>
              {check.buyer && (
                <tr><td>买方</td><td colSpan={3}>
                  {check.buyer.name || '账号不存在'}（{check.buyer.unit_no || ''}）
                  {check.buyer.is_building_member ? <span className="badge blue" style={{ marginLeft: 6 }}>本楼栋住户</span>
                    : <span className="badge gray" style={{ marginLeft: 6 }}>非本楼栋成员</span>}
                  {check.buyer.accepted
                    ? <span className="badge green" style={{ marginLeft: 6 }}>已确认接受权益</span>
                    : <span className="badge amber" style={{ marginLeft: 6 }}>尚未接受权益</span>}
                </td></tr>
              )}
            </tbody>
          </table>
        )}
        {check && check.blockers?.length > 0 && (
          <div className="muted-box" style={{ marginTop: 8, borderLeft: '3px solid #d97706' }}>
            <b>办理前需先处理：</b>
            <ul style={{ margin: '6px 0 0 18px' }}>
              {check.blockers.map((b, i) => <li key={i}>{b}</li>)}
            </ul>
          </div>
        )}
        {check && check.blockers?.length === 0 && (
          <div className="small" style={{ marginTop: 8, color: '#16a34a' }}>核查通过，无阻断项，可发起申请并进入买方接受 / 物业材料审核流程。</div>
        )}
      </div>

      {/* 发起处理 */}
      <div className="card">
        <h2>发起认购权益处理</h2>
        <div className="form-row">
          <label className="field">
            <span>认购记录</span>
            <select value={form.subscription_id} onChange={(e) => setForm({ ...form, subscription_id: Number(e.target.value) })}>
              <option value={0}>请选择…</option>
              {paidSubs.map((s) => (
                <option key={s.id} value={s.id}>{s.name}（{s.unit_no}）· {Number(s.ratio).toFixed(1)}% · ¥{s.amount}</option>
              ))}
            </select>
          </label>
          <label className="field">
            <span>处理方式</span>
            <select value={form.deal_type} onChange={(e) => setForm({ ...form, deal_type: e.target.value, to_user_id: 0 })}>
              <option value="follow_house">随房转让给买方（权益随房，下一期起归新业主）</option>
              <option value="early_exit">提前结算退出（退还认购款，冲红票据，下调楼栋认购比例）</option>
              <option value="transfer_neighbor">转给同楼栋其他住户</option>
            </select>
          </label>
          {form.deal_type !== 'early_exit' && (
            <label className="field">
              <span>受让方（已注册住户，需本人确认接受）</span>
              <select value={form.to_user_id} onChange={(e) => setForm({ ...form, to_user_id: Number(e.target.value) })}>
                <option value={0}>请选择…</option>
                {buyerCandidates
                  .filter((u) => form.deal_type === 'follow_house' || memberIDs.has(u.id))
                  .map((u) => (
                    <option key={u.id} value={u.id}>
                      {u.name}（{u.unit_no || u.phone}）{memberIDs.has(u.id) ? '' : '· 非本楼栋'}
                    </option>
                  ))}
              </select>
            </label>
          )}
        </div>
        <label className="field"><span>备注</span><input value={form.memo} onChange={(e) => setForm({ ...form, memo: e.target.value })} /></label>
        <div className="small muted" style={{ margin: '6px 0 10px' }}>
          {form.deal_type === 'follow_house' && '权益随房转移：历史已结算收益归原业主，自物业审核通过后的下一生效月起按新份额向买方发放收益；开具权益转让凭证。'}
          {form.deal_type === 'early_exit' && '提前结算退出：退还全额认购款并冲红原认购票据，该户退出认购，楼栋已认购比例相应下调；历史已结算收益仍归原业主，可继续提现。'}
          {form.deal_type === 'transfer_neighbor' && '转给同楼栋住户：仅可选本楼栋成员；受让方接受、物业审核材料后，下一期收益改向受让方发放。'}
        </div>
        <button className="primary" onClick={submit}>提交申请</button>
        {paidSubs.length === 0 && <div className="small muted" style={{ marginTop: 8 }}>当前没有可办理的已缴费认购。</div>}
      </div>

      {/* 转让记录 */}
      <div className="card">
        <h2>转让 / 退出办理记录</h2>
        <table>
          <thead>
            <tr>
              <th>申请时间</th><th>原业主</th><th>受让方/方式</th><th>买方接受</th><th>历史收益</th>
              <th>材料审核 / 生效月</th><th>状态</th>{isManager && <th>操作</th>}
            </tr>
          </thead>
          <tbody>
            {transfers.map((t) => {
              const iAmBuyer = t.to_user_id && t.to_user_id === user.id
              return (
                <tr key={t.id}>
                  <td className="small">{new Date(t.created_at).toLocaleDateString('zh-CN')}</td>
                  <td>{t.from_name}<div className="small muted">{t.from_unit} · {Number(t.panel_ratio).toFixed(1)}%</div></td>
                  <td>
                    <div><b>{DEAL_TYPE_LABEL[t.deal_type] || t.deal_type}</b></div>
                    {t.deal_type === 'early_exit'
                      ? <span className="small muted">退还认购款并退出</span>
                      : <span className="small">{t.to_name || '—'}<span className="muted">（{t.to_unit}）</span></span>}
                  </td>
                  <td>{t.deal_type === 'early_exit'
                    ? <span className="muted">—</span>
                    : t.buyer_accepted ? <span className="badge green">已接受</span> : <span className="badge amber">待接受</span>}</td>
                  <td>{t.history_settled ? <span className="badge green">已结清</span> : <span className="badge amber">待结清</span>}</td>
                  <td className="small">
                    {t.status === 'confirmed'
                      ? <>生效月 <b>{t.effective_month}</b><div className="muted">材料：{t.documents}</div>{t.review_memo && <div className="muted">意见：{t.review_memo}</div>}</>
                      : <span className="muted">待物业审核过户材料</span>}
                  </td>
                  <td>
                    <Badge value={t.status === 'confirmed' ? 'paid' : t.status === 'rejected' ? 'refunded' : 'pending'} />
                    <div className="small muted">{t.status === 'confirmed' ? '已办理' : t.status === 'pending' ? '待审核' : '已驳回'}</div>
                  </td>
                  <td>
                    <div className="btn-row">
                      {t.status === 'pending' && iAmBuyer && !t.buyer_accepted && (
                        <button className="sm primary" onClick={() => accept(t)}>我接受权益</button>
                      )}
                      {t.status === 'pending' && isManager && (
                        <>
                          <button className="sm primary" onClick={() => confirm(t)}>材料审核通过</button>
                          <button className="sm danger" onClick={() => reject(t)}>驳回</button>
                        </>
                      )}
                      {(t.from_user_id === user.id || isManager) && (
                        <button className="sm" onClick={() => exportCSV(t.subscription_id, t.from_unit)}>交割附件</button>
                      )}
                    </div>
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
        {transfers.length === 0 && <div className="list-empty">暂无转让/退出记录</div>}
      </div>

      {/* 收益提现 */}
      <div className="card">
        <h2>收益账户与提现</h2>
        {!isManager && (
          <>
            <div className="muted-box" style={{ marginBottom: 12 }}>
              本楼栋可提余额：<b>¥{fmtMoney(myAvailable)}</b>
              <span className="small muted">（已定稿结算净收益，扣除审核中/已放款的提现）</span>
            </div>
            <div className="form-row">
              <label className="field"><span>提现金额（元）</span>
                <input type="number" min="0" step="0.01" value={wdForm.amount}
                  onChange={(e) => setWdForm({ ...wdForm, amount: e.target.value })} /></label>
              <label className="field"><span>收款账户（开户行/卡号）</span>
                <input placeholder="如：工商银行 6217…6011" value={wdForm.bank_info}
                  onChange={(e) => setWdForm({ ...wdForm, bank_info: e.target.value })} /></label>
            </div>
            <button className="primary" onClick={submitWithdrawal}>申请提现</button>
          </>
        )}
        <table style={{ marginTop: 10 }}>
          <thead>
            <tr><th>申请时间</th>{isManager && <th>住户</th>}<th>金额</th><th>收款账户</th><th>状态</th><th>备注</th>{isManager && <th>操作</th>}</tr>
          </thead>
          <tbody>
            {withdrawals.items?.map((w) => (
              <tr key={w.id}>
                <td className="small">{new Date(w.created_at).toLocaleString('zh-CN')}</td>
                {isManager && <td>{w.name}<div className="small muted">可提 ¥{fmtMoney(withdrawals.available?.[String(w.user_id)] ?? 0)}</div></td>}
                <td>¥{fmtMoney(w.amount)}</td>
                <td className="small">{w.bank_info || '—'}</td>
                <td>{w.status === 'paid'
                  ? <span className="badge green">已放款</span>
                  : w.status === 'rejected' ? <span className="badge red">已驳回</span>
                  : <span className="badge amber">待审核</span>}</td>
                <td className="small muted">{w.reject_memo || (w.paid_at ? `放款 ${w.paid_at.slice(0, 16)}` : '')}</td>
                {isManager && (
                  <td>{w.status === 'pending' && (
                    <div className="btn-row">
                      <button className="sm primary" onClick={() => reviewWD(w, true)}>放款</button>
                      <button className="sm danger" onClick={() => reviewWD(w, false)}>驳回</button>
                    </div>
                  )}</td>
                )}
              </tr>
            ))}
          </tbody>
        </table>
        {(!withdrawals.items || withdrawals.items.length === 0) && <div className="list-empty">暂无提现记录</div>}
      </div>

      {/* 票据 */}
      <div className="card">
        <h2>票据（认购 / 收益 / 退款冲红 / 权益转让）</h2>
        <table>
          <thead>
            <tr><th>开票时间</th><th>票据号码</th><th>类型</th><th>抬头</th><th>项目内容</th><th>金额</th><th>期间/业务</th><th>状态</th>{isManager && <th>操作</th>}</tr>
          </thead>
          <tbody>
            {invoices.map((inv) => (
              <tr key={inv.id}>
                <td className="small">{new Date(inv.created_at).toLocaleDateString('zh-CN')}</td>
                <td className="small">{inv.invoice_no}</td>
                <td>{inv.invoice_type === 'red'
                  ? <span className="badge red">红字冲销</span>
                  : <span className="badge blue">蓝字凭证</span>}</td>
                <td>{inv.buyer_name || '—'}</td>
                <td className="small">{inv.item_name}</td>
                <td>¥{fmtMoney(inv.amount)}</td>
                <td className="small muted">{inv.period_month || ''} {inv.period_month ? '·' : ''}
                  {({ subscription: '认购缴费', settlement: '月度收益', refund: '退款冲红', transfer: '权益转让' })[inv.ref_type] || inv.ref_type}</td>
                <td>{inv.status === 'void'
                  ? <span className="badge red">已作废</span>
                  : <span className="badge green">已开具</span>}</td>
                {isManager && (
                  <td>{inv.invoice_type === 'normal' && inv.status === 'issued' && (
                    <button className="sm danger" onClick={() => voidInvoice(inv)}>作废/冲红</button>
                  )}</td>
                )}
              </tr>
            ))}
          </tbody>
        </table>
        {invoices.length === 0 && <div className="list-empty">暂无票据</div>}
      </div>
    </>
  )
}
