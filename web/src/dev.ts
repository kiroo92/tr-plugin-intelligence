import { configureMock } from '@tokenflux/tokenrouter-plugin-sdk/dev'
// dev.ts supplies in-memory configuration, diagnostic, and artwork responses
// for local page review. The fixtures include an expired subscription and a
// retained drawing so history and preview interactions can be checked without
// connecting a host or making paid model requests.
const configs = [
  { id: 1, group_id: 12, group_name: 'default', model: 'gpt-6-astra', benchmark: 'candy', prompt: '', base_url: 'https://relay.example.com/v1', protocol: 'responses', reasoning_effort: 'max', service_tier: '', enabled: true, schedule_enabled: true, interval_minutes: 60, next_run_at: Math.floor(Date.now() / 1000) + 1800, key_set: true, created_at: 0, updated_at: 0 },
  { id: 2, group_id: 12, group_name: 'default', model: 'gpt-6-astra', benchmark: 'drawing', prompt: '', base_url: 'https://relay.example.com/v1', protocol: 'responses', reasoning_effort: 'high', service_tier: 'priority', enabled: true, schedule_enabled: true, interval_minutes: 180, next_run_at: Math.floor(Date.now() / 1000) + 3600, key_set: true, created_at: 0, updated_at: 0 },
  { id: 3, group_id: 31, group_name: 'claude 专线', model: 'claude-fable-5-2', benchmark: 'candy', prompt: '', base_url: 'https://relay.example.com/v1', protocol: 'responses', reasoning_effort: 'xhigh', service_tier: '', enabled: true, schedule_enabled: false, interval_minutes: 60, next_run_at: 0, key_set: false, created_at: 0, updated_at: 0 },
] as const
const now = Math.floor(Date.now() / 1000)
function run(id: string, configId: number, verdict: string, minutes: number, extra: Record<string, unknown> = {}) {
  return { id, config_id: configId, group_id: 12, model: 'gpt-6-astra', benchmark: 'candy', status: verdict === 'passed' || verdict === 'failed' ? 'succeeded' : verdict === 'error' ? 'failed' : 'succeeded', verdict, phase: 'complete', has_html: false, created_at: now - minutes * 60, updated_at: now - minutes * 60, finished_at: now - minutes * 60, ...extra }
}
const history = [
  run('r1', 1, 'passed', 4, { duration_ms: 31000, first_token_ms: 820, input_tokens: 980, output_tokens: 640 }),
  run('r2', 1, 'passed', 64),
  run('r3', 1, 'failed', 124, { assessment_reason: '答案 20，与期望 21 不符' }),
  run('r4', 1, 'error', 184, { error_message: '模型接口余额或额度不足（HTTP 429）' }),
  run('r5', 1, 'unknown', 244, { error_message: '评价缺失或无法判定' }),
]
// A denser second history shows the strip filling up and the counts changing.
for (let index = 0; index < 22; index++) {
  history.push(run(`d${index}`, 1, index % 5 === 0 ? 'failed' : 'passed', 300 + index * 60))
}
// The drawing row shows an in-flight task so the phase plate is reviewable.
const inflight = {
  id: 'live', config_id: 2, group_id: 12, model: 'gpt-6-astra', benchmark: 'drawing',
  status: 'running', verdict: 'pending', phase: 'generating', has_html: false,
  created_at: now - 120, updated_at: now - 30, finished_at: 0,
}
// drawing is a retained artifact whose scripts and SVG animation exercise the
// same isolated preview used for documents returned by the authenticated API.
const drawing = run('drawing-art', 2, 'passed', 15, { benchmark: 'drawing', has_html: true, duration_ms: 42000 })
// expired models a permanent gateway rejection before generation. It stays in
// administrator diagnostics while the last valid quality conclusion remains visible.
const expired = run('expired', 1, 'error', 1, {
  error_code: 'subscription_expired',
  error_message: '检测 Key 所属订阅已过期，请续订或更换可用 Key（HTTP 403）',
})
const artwork = `<!doctype html><html><head><meta charset="utf-8"><style>
body{margin:0;display:grid;place-items:center;min-height:100vh;background:#edf7fb;color:#173b55;font:18px system-ui}
main{text-align:center}svg{width:min(90vw,480px)}circle{transform-origin:240px 120px;animation:orbit 5s linear infinite}
@keyframes orbit{to{transform:rotate(360deg)}}
</style></head><body><main><svg viewBox="0 0 480 240"><ellipse cx="240" cy="120" rx="150" ry="85" fill="none" stroke="#8fc9db" stroke-width="4"/><circle cx="240" cy="35" r="16" fill="#e3ab35"/><path d="M180 150 Q240 40 300 150" fill="none" stroke="#2383a3" stroke-width="12" stroke-linecap="round"/></svg><p id="ready">HTML 作品</p></main><script>document.getElementById('ready').textContent='HTML 动画预览';</script></body></html>`
const views = configs.map((config, index) => ({
  config,
  latest: index === 1 ? inflight : index === 0 ? expired : (history[index] ?? undefined),
  latest_judged: index === 1 ? drawing : history[index] ?? undefined,
  history: index === 1 ? [drawing] : history.filter(entry => entry.config_id === config.id && (entry.verdict === 'passed' || entry.verdict === 'failed')).slice().reverse(),
  pass_rate: 66,
  judged: 3,
  active: index === 1,
  rate_multiplier: config.group_id === 31 ? 1.35 : 1,
  // An in-flight task keeps its previous judgement beside the current phase.
}))
configureMock(async (method, path) => {
  if (path === '/admin/configs') return { enabled: true, master_key_set: true, configs: views }
  if (path === '/admin/settings') return { enabled: true, master_key_set: true }
  if (path === '/admin/groups') return { groups: [
    { id: 12, name: 'default', rate_multiplier: 1, models: [
      { id: 'gpt-6-astra', name: 'GPT-6 Astra' },
      { id: 'claude-fable-5-2', name: 'Claude Fable 5.2' },
      { id: 'claude-sonnet-5-5', name: 'Claude Sonnet 5.5' },
    ] },
    { id: 31, name: 'claude 专线', rate_multiplier: 1.35, models: [
      { id: 'claude-fable-5-2', name: 'Claude Fable 5.2' },
    ] },
  ] }
  if (path === '/tests') return { groups: [{ id: 12, name: 'default', rate_multiplier: 1, configs: views.slice(0, 2) }] }
  if (/^\/admin\/configs\/\d+\/runs$/.test(path)) {
    const configId = Number(path.split('/')[3])
    return { runs: [...history, drawing, expired].filter(entry => entry.config_id === configId).sort((left, right) => right.created_at - left.created_at) }
  }
  if (path.startsWith('/admin/runs/') || path.startsWith('/runs/')) {
    const id = path.split('/').at(-1)
    const selected = [...history, drawing, expired].find(entry => entry.id === id)
    return { run: selected, html: selected?.has_html ? artwork : '' }
  }
  if (method === 'POST' || method === 'PUT' || method === 'DELETE') return { saved: true }
  return {}
})
