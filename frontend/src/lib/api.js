// 极简 API 封装：JWT 存 localStorage，错误统一抛出 Error(message)
const TOKEN_KEY = 'pv_token'

export function getToken() {
  return localStorage.getItem(TOKEN_KEY) || ''
}
export function setToken(t) {
  localStorage.setItem(TOKEN_KEY, t)
}
export function clearToken() {
  localStorage.removeItem(TOKEN_KEY)
}

export async function api(method, path, body) {
  const headers = { 'Content-Type': 'application/json' }
  const token = getToken()
  if (token) headers.Authorization = 'Bearer ' + token
  const resp = await fetch('/api' + path, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })
  const text = await resp.text()
  let data = {}
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = { raw: text }
    }
  }
  if (!resp.ok) {
    throw new Error(data.error || `请求失败 (${resp.status})`)
  }
  return data
}

export const fmtMoney = (v) =>
  Number(v || 0).toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 })
export const fmtNum = (v, d = 0) =>
  Number(v || 0).toLocaleString('zh-CN', { minimumFractionDigits: d, maximumFractionDigits: d })

// 带 JWT 的文件下载（用于交割附件 CSV 导出）
export async function apiDownload(path, fallbackName = 'export.csv') {
  const resp = await fetch('/api' + path, {
    headers: { Authorization: 'Bearer ' + getToken() },
  })
  if (!resp.ok) {
    const text = await resp.text()
    let msg = `下载失败 (${resp.status})`
    try { msg = JSON.parse(text).error || msg } catch { /* ignore */ }
    throw new Error(msg)
  }
  const blob = await resp.blob()
  const cd = resp.headers.get('Content-Disposition') || ''
  const m = cd.match(/filename="?([^"]+)"?/)
  const name = m ? decodeURIComponent(m[1]) : fallbackName
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = name
  document.body.appendChild(a)
  a.click()
  a.remove()
  URL.revokeObjectURL(url)
}

export const DEAL_TYPE_LABEL = {
  follow_house: '随房转让',
  early_exit: '提前结算退出',
  transfer_neighbor: '转给同楼栋住户',
}

export const STAGES = [
  { key: 'draft', label: '立项建档' },
  { key: 'voting', label: '楼栋表决' },
  { key: 'subscribing', label: '认购缴费' },
  { key: 'construction', label: '施工排期' },
  { key: 'grid_acceptance', label: '并网验收' },
  { key: 'operating', label: '发电运营' },
]
export const stageLabel = (k) => STAGES.find((s) => s.key === k)?.label || k

export const ROLE_LABEL = { admin: '管理员', property: '物业', resident: '住户' }
export const WO_RESP = {
  undetermined: '待判定',
  property: '物业',
  installer: '安装商',
  owner: '业主',
  insurance: '保险',
}
