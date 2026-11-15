import { configureMock } from '@tokenflux/tokenrouter-plugin-sdk/dev'
// The development mock serves in-memory fixtures so the page layout can be
// reviewed without a host or a real detection run.
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
const views = configs.map((config, index) => ({
  config,
  latest: index === 1 ? inflight : (history[index] ?? undefined),
  latest_judged: history[index] ?? undefined,
  history: history.filter(entry => entry.config_id === config.id).slice().reverse(),
  pass_rate: 66,
  judged: 3,
  active: index === 1,
  rate_multiplier: config.group_id === 31 ? 1.35 : 1,
  // An in-flight task keeps its previous judgement beside the current phase.
}))
configureMock(async (method, path) => {
  if (path === '/admin/configs') return { enabled: true, master_key_set: true, configs: views }
  if (path === '/admin/settings') return { enabled: true, master_key_set: true }
  if (path === '/tests') return { groups: [{ id: 12, name: 'default', rate_multiplier: 1, configs: views.slice(0, 2) }] }
  if (path.startsWith('/admin/runs/')) return { run: history[0], html: '<!doctype html><html><body>本地预览作品</body></html>' }
  if (path.startsWith('/runs/')) return { run: history[0], html: '<!doctype html><html><body>本地预览作品</body></html>' }
  if (method === 'POST' || method === 'PUT' || method === 'DELETE') return { saved: true }
  return {}
})
