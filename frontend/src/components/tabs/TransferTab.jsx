import React, { useEffect, useState } from 'react'
import { api, fmtMoney, getToken } from '../../lib/api'
import { Badge, handleErr, useToast } from '../ui'

const MODE_LABEL = {
  with_house: '随房转让',
  early_settlement: '提前结算退出',
  to_resident: '转给同楼栋住户',
}
const MODE_BADGE = { with_house: 'blue', early_settlement: 'red', to_resident: 'amber' }
const STATUS_LABEL = { pending: '待审核', confirmed: '已办理', rejected: '已驳回', cancelled: '已取消' }
const STATUS_BADGE = { pending: 'amber', confirmed: 'green', rejected: 'red', cancelled: 'gray' }
const INV_TYPE = { subscription: '认购发票', reissue: '换开发票', refund: '退款凭证' }
const INV_STATUS = { valid: '有效', voided: '已红冲', replaced: '已换开作废' }

const MATERIAL_OPTIONS = [
  '房屋买卖合同',
  '不动产权证（或过户受理单）复印件',
  '买卖双方身份证明',
  '光伏认购权益受让确认书',
]

// 下载交割附件（携带 JWT）
async function downloadFile(tid, format, notify) {
  try {
    const resp = await fetch(`/api/transfers/${tid}/export?format=${format}`, {
      headers: { Authorization: 'Bearer ' + getToken() },
    })
    if (!resp.ok) {
      const data = await resp.json().catch(() => ({}))
      throw new Error(data.error || `导出失败 (${resp.status})`)
    }
    const blob = await resp.blob()
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `transfer-${tid}-delivery.${format === 'csv' ? 'csv' : 'json'}`
    a.click()
    URL.revokeObjectURL(url)
  } catch (e) {
    notify(e.message, true)
  }
}

export default function TransferTab({ user, building }) {
  const { notify } = useToast()
  const isManager = user.role === 'admin' || user.role === 'property'
  const [subs, setSubs] = useState([])
  const [transfers, setTransfers] = useState([])
  const [users, setUsers] = useState([])
  const [members, setMembers] = useState([])
  const [invoices, setInvoices] = useState([])
  const [check, setCheck] = useState(null)
  const [form, setForm] = useState({
    subscription_id: 0, mode: 'with_house', to_user_id: 0, to_name: '', to_unit: '',
    materials: [], memo: '',
  })

  const load = async () => {
    const [s, t, m, inv] = await Promise.all([
      api('GET', `/buildings/${building.id}/subscriptions`),
      api('GET', `/buildings/${building.id}/transfers`),
      api('GET', `/buildings/${building.id}/members`),
      api('GET', `/buildings/${building.id}/invoices`).catch(() => ({ items: [] })),
    ])
    setSubs(s.items || [])
    setTransfers(t.items || [])
    setMembers(m.items || [])
    setInvoices(inv.items || [])
    if (isManager) {
      const u = await api('GET', '/users').catch(() => ({ items: [] }))
      setUsers(u.items || [])
    }
  }
  useEffect(() => { load() }, [building.id])

  // 转让前检查
  const loadCheck = async (sid) => {
    if (!sid) { setCheck(null); return }
    try {
      setCheck(await handleErr(api('GET', `/subscriptions/${sid}/transfer-check`), notify))
    } catch { setCheck(null) }
  }
  useEffect(() => { loadCheck(form.subscription_id) }, [form.subscription_id])

  const paidSubs = subs.filter((x) => x.status === 'paid' && x.type === 'shareholder'
    && (isManager || x.user_id === user.id))
  const residents = members.filter((m) => m.member_type === 'resident')
  const modeInfo = check?.modes?.[form.mode]

  const toggleMaterial = (m) => {
    const has = form.materials.includes(m)
    setForm({ ...form, materials: has ? form.materials.filter((x) => x !== m) : [...form.materials, m] })
  }

  const submit = async () => {
    if (!form.subscription_id) return notify('请选择要办理的认购记录', true)
    if (form.mode === 'with_house' && !form.to_user_id && !form.to_name) {
      return notify('随房转让需选择买方（已注册用户）或填写买方姓名', true)
    }
    if (form.mode === 'to_resident' && !form.to_user_id) {
      return notify('转给同楼栋住户需选择受让住户', true)
    }
    try {
      await handleErr(api('POST', `/buildings/${building.id}/transfers`, {
        ...form, to_user_id: form.mode === 'early_settlement' ? 0 : form.to_user_id,
      }), notify)
      notify('申请已提交：买方接受权益并经物业审核过户材料后，下一期收益按新权益人发放')
      setForm({ subscription_id: 0, mode: 'with_house', to_user_id: 0, to_name: '', to_unit: '', materials: [], memo: '' })
      setCheck(null)
      load()
    } catch { /* toast */ }
  }

  const accept = async (t, ok) => {
    const note = ok ? '' : (window.prompt('拒绝原因（可选）：', '') ?? '')
    try {
      await handleErr(api('POST', `/transfers/${t.id}/accept`, { accept: ok, note }), notify)
      notify(ok ? '已确认接受认购权益' : '已拒绝，申请取消')
      load()
    } catch { /* toast */ }
  }
  const confirm = async (t) => {
    const note = window.prompt('审核意见（可选，确认前请核对过户材料与历史收益结清情况）：', '')
    if (note === null) return
    try {
      const r = await handleErr(api('POST', `/transfers/${t.id}/confirm`, { review_note: note }), notify)
      notify(`办理完成${r.invoice_no ? '，发票 ' + r.invoice_no : ''}`)
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
  const dispute = async (t) => {
    const note = window.prompt('争议说明（将写入交割附件）：', '')
    if (!note) return
    try {
      await handleErr(api('POST', `/transfers/${t.id}/dispute`, { note }), notify)
      notify('争议已登记')
      load()
    } catch { /* toast */ }
  }

  const involved = (t) => isManager || t.from_user_id === user.id || t.to_user_id === user.id

  return (
    <>
      <div className="card">
        <h2>房屋出售与认购权益处理</h2>
        <div className="muted-box" style={{ marginBottom: 12 }}>
          出售房屋时平台自动核查该户<b>认购份额、未结收益、维修分摊</b>与<b>买方是否接受权益</b>。
          三种处理方式：①<b>随房转让</b>——权益过户给买方，楼栋认购比例不变，认购发票换开；
          ②<b>提前结算退出</b>——历史收益结清后认购款退还至卖方收益账户，原发票红冲并开退款凭证，楼栋认购比例下降；
          ③<b>转给同楼栋住户</b>——权益转给本楼其他住户，比例不变。
          物业审核过户材料后办理，<b>下一期收益才按新权益人发放</b>；对历史收益有争议可导出交割附件。
        </div>

        <div className="form-row">
          <label className="field">
            <span>认购记录（转让前自动核查）</span>
            <select value={form.subscription_id}
              onChange={(e) => setForm({ ...form, subscription_id: Number(e.target.value) })}>
              <option value={0}>请选择…</option>
              {paidSubs.map((s) => (
                <option key={s.id} value={s.id}>{s.name}（{s.unit_no}）· {Number(s.ratio).toFixed(1)}% · ¥{fmtMoney(s.amount)}</option>
              ))}
            </select>
          </label>
          <label className="field">
            <span>处理方式</span>
            <select value={form.mode} onChange={(e) => setForm({ ...form, mode: e.target.value, to_user_id: 0 })}>
              <option value="with_house">随房转让（过户给买方）</option>
              <option value="early_settlement">提前结算退出（退还认购款）</option>
              <option value="to_resident">转给同楼栋其他住户</option>
            </select>
          </label>
          {form.mode === 'with_house' && (
            <label className="field">
              <span>买方（已注册用户）</span>
              <select value={form.to_user_id} onChange={(e) => setForm({ ...form, to_user_id: Number(e.target.value) })}>
                <option value={0}>请选择或在下方填写…</option>
                {(isManager ? users.filter((u) => u.role === 'resident') : residents)
                  .filter((u) => u.id !== user.id)
                  .map((u) => <option key={u.id} value={u.id}>{u.name}（{u.unit_no || u.phone}）</option>)}
              </select>
            </label>
          )}
          {form.mode === 'to_resident' && (
            <label className="field">
              <span>受让住户（本楼栋）</span>
              <select value={form.to_user_id} onChange={(e) => setForm({ ...form, to_user_id: Number(e.target.value) })}>
                <option value={0}>请选择…</option>
                {residents.filter((u) => u.id !== user.id)
                  .map((u) => <option key={u.id} value={u.id}>{u.name}（{u.unit_no}）</option>)}
              </select>
            </label>
          )}
        </div>
        {form.mode === 'with_house' && !form.to_user_id && (
          <div className="form-row">
            <label className="field"><span>买方姓名（未注册时）</span><input value={form.to_name} onChange={(e) => setForm({ ...form, to_name: e.target.value })} /></label>
            <label className="field"><span>买方房号</span><input value={form.to_unit} onChange={(e) => setForm({ ...form, to_unit: e.target.value })} /></label>
          </div>
        )}

        {check && (
          <div className="muted-box" style={{ marginBottom: 12 }}>
            <b>转让前检查：</b>
            认购 {Number(check.subscription.ratio).toFixed(2)}%（{check.subscription.panel_count} 块，¥{fmtMoney(check.subscription.amount)}）；
            收益账户余额 ¥{fmtMoney(check.balance)}；
            未结收益 {check.unsettled.draft_count} 期{check.unsettled.draft_count > 0 && `（合计 ¥${fmtMoney(check.unsettled.total)}，须先定稿结清）`}；
            维修分摊敞口 {check.maintenance.open_count} 单{check.maintenance.open_count > 0 && `（工单费用合计 ¥${fmtMoney(check.maintenance.exposure)}）`}。
            {!check.can_transfer && <div style={{ color: '#b91c1c' }}>暂不可办理：{check.blockers.join('；')}</div>}
            {modeInfo && (
              <table style={{ marginTop: 8 }}>
                <thead><tr><th>当前方式影响</th><th>内容</th></tr></thead>
                <tbody>
                  <tr><td>楼栋认购比例</td><td>{Number(check.building.subscribed_ratio).toFixed(2)}% → {Number(modeInfo.ratio_after).toFixed(2)}%</td></tr>
                  <tr><td>收益账户</td><td>{modeInfo.account_effect}</td></tr>
                  <tr><td>发票</td><td>{modeInfo.invoice_effect}</td></tr>
                  <tr><td>收益发放</td><td>{modeInfo.earnings}</td></tr>
                </tbody>
              </table>
            )}
          </div>
        )}

        <div className="field">
          <span>过户材料（勾选已备齐项，物业审核时核对）</span>
          <div className="btn-row" style={{ flexWrap: 'wrap', gap: 8 }}>
            {MATERIAL_OPTIONS.map((m) => (
              <label key={m} className="small" style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
                <input type="checkbox" checked={form.materials.includes(m)} onChange={() => toggleMaterial(m)} /> {m}
              </label>
            ))}
          </div>
        </div>
        <label className="field"><span>备注</span><input value={form.memo} onChange={(e) => setForm({ ...form, memo: e.target.value })} /></label>
        <button className="primary" onClick={submit}>提交申请</button>
        {paidSubs.length === 0 && <div className="small muted" style={{ marginTop: 8 }}>当前没有可办理的已缴费认购。</div>}
      </div>

      <div className="card">
        <h2>转让 / 退出记录</h2>
        <table>
          <thead>
            <tr>
              <th>申请时间</th><th>卖方 → 受让方</th><th>方式</th><th>买方接受</th>
              <th>状态</th><th>材料 / 备注 / 争议</th><th>操作</th>
            </tr>
          </thead>
          <tbody>
            {transfers.map((t) => (
              <tr key={t.id}>
                <td className="small">{new Date(t.created_at).toLocaleDateString('zh-CN')}</td>
                <td>
                  {t.from_name}<span className="muted small">（{t.from_unit}）</span>
                  {t.mode !== 'early_settlement' && <> → {t.to_name || '—'}<span className="muted small">（{t.to_unit}）</span></>}
                  <div className="small muted">{Number(t.sub_ratio).toFixed(1)}% · ¥{fmtMoney(t.sub_amount)}</div>
                </td>
                <td><span className={'badge ' + MODE_BADGE[t.mode]}>{MODE_LABEL[t.mode]}</span></td>
                <td>
                  {t.mode === 'early_settlement' ? <span className="muted small">无需</span>
                    : t.buyer_accepted ? <span className="badge green">已接受</span>
                      : <span className="badge amber">待接受</span>}
                </td>
                <td><span className={'badge ' + STATUS_BADGE[t.status]}>{STATUS_LABEL[t.status]}</span></td>
                <td className="small">
                  {t.materials?.length > 0 && <div>材料：{t.materials.join('、')}</div>}
                  {t.memo && <div className="muted">{t.memo}</div>}
                  {t.dispute_note && <div style={{ color: '#b91c1c', whiteSpace: 'pre-line' }}>争议：{t.dispute_note}</div>}
                  {t.review_note && <div className="muted" style={{ whiteSpace: 'pre-line' }}>{t.review_note}</div>}
                </td>
                <td>
                  <div className="btn-row" style={{ flexWrap: 'wrap' }}>
                    {t.status === 'pending' && t.to_user_id === user.id && !t.buyer_accepted && (
                      <>
                        <button className="sm primary" onClick={() => accept(t, true)}>接受权益</button>
                        <button className="sm danger" onClick={() => accept(t, false)}>拒绝</button>
                      </>
                    )}
                    {t.status === 'pending' && isManager && (
                      <>
                        {t.mode !== 'early_settlement' && !t.buyer_accepted && (
                          <button className="sm" onClick={() => accept(t, true)}>代录买方接受</button>
                        )}
                        <button className="sm primary" onClick={() => confirm(t)}>审核办理</button>
                        <button className="sm danger" onClick={() => reject(t)}>驳回</button>
                      </>
                    )}
                    {involved(t) && (
                      <>
                        <button className="sm" onClick={() => dispute(t)}>登记争议</button>
                        <button className="sm" onClick={() => downloadFile(t.id, 'csv', notify)}>交割附件CSV</button>
                        <button className="sm" onClick={() => downloadFile(t.id, 'json', notify)}>JSON</button>
                      </>
                    )}
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {transfers.length === 0 && <div className="list-empty">暂无转让/退出记录</div>}
      </div>

      <div className="card">
        <h2>发票记录{!isManager && '（仅本人）'}</h2>
        <table>
          <thead>
            <tr><th>发票号</th><th>类型</th><th>抬头</th><th className="num">金额</th><th>状态</th><th>开具时间</th><th>备注</th></tr>
          </thead>
          <tbody>
            {invoices.map((v) => (
              <tr key={v.id}>
                <td>{v.invoice_no}</td>
                <td>{INV_TYPE[v.inv_type] || v.inv_type}</td>
                <td>{v.holder_name}</td>
                <td className="num">¥{fmtMoney(v.amount)}</td>
                <td>
                  <span className={'badge ' + (v.status === 'valid' ? 'green' : v.status === 'voided' ? 'red' : 'gray')}>
                    {INV_STATUS[v.status] || v.status}
                  </span>
                </td>
                <td className="small">{new Date(v.created_at).toLocaleDateString('zh-CN')}</td>
                <td className="small muted">{v.memo}</td>
              </tr>
            ))}
          </tbody>
        </table>
        {invoices.length === 0 && <div className="list-empty">暂无发票记录</div>}
      </div>
    </>
  )
}
