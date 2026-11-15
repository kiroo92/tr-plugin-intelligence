// types.ts describes configuration, observations, and retained artwork returned
// by the plugin's JSON API. Both page audiences share these shapes, while
// administrator history includes diagnostics and quality strips select only
// observations with a definite verdict.

export interface Config {
  id: number
  group_id: number
  group_name: string
  model: string
  benchmark: 'candy' | 'drawing'
  prompt: string
  base_url: string
  protocol: 'responses' | 'chat_completions'
  reasoning_effort: string
  service_tier: string
  enabled: boolean
  schedule_enabled: boolean
  interval_minutes: number
  next_run_at: number
  key_set: boolean
  created_at: number
  updated_at: number
}

export type Verdict = 'pending' | 'passed' | 'failed' | 'unknown' | 'error'
export type RunStatus = 'queued' | 'running' | 'succeeded' | 'failed' | 'cancelled'

export interface Run {
  id: string
  config_id: number
  group_id: number
  model: string
  benchmark: 'candy' | 'drawing'
  status: RunStatus
  verdict: Verdict
  phase: string
  question?: string
  answer?: string
  input_tokens?: number
  output_tokens?: number
  reasoning_tokens?: number
  duration_ms?: number
  first_token_ms?: number
  assessment_reason?: string
  error_code?: string
  error_message?: string
  has_html: boolean
  created_at: number
  updated_at: number
  finished_at?: number
}

export interface ConfigView {
  config: Config
  latest?: Run
  latest_judged?: Run
  history: Run[]
  pass_rate?: number
  judged: number
  active: boolean
  rate_multiplier: number
}

/** CatalogModel is one requestable model identifier offered by a group. */
export interface CatalogModel {
  id: string
  name: string
}

/** CatalogGroup is a visible group with its multiplier and listed models. */
export interface CatalogGroup {
  id: number
  name: string
  rate_multiplier: number
  models: CatalogModel[]
}

export interface AdminList {
  enabled: boolean
  master_key_set: boolean
  configs: ConfigView[]
}

export interface Settings {
  enabled: boolean
  master_key_set: boolean
}

export interface RunDetail {
  run: Run
  // html carries the saved document as text for the isolated preview, escaped
  // source view, and download. The host application never inserts it into its DOM.
  html?: string
}

export interface UserGroup {
  id: number
  name: string
  rate_multiplier: number
  configs: ConfigView[]
}

export interface UserTests {
  groups: UserGroup[]
}

export interface ConfigInput {
  group_id: number
  group_name: string
  model: string
  benchmark: 'candy' | 'drawing'
  prompt: string
  base_url: string
  api_key: string
  protocol: 'responses' | 'chat_completions'
  reasoning_effort: string
  service_tier: string
  enabled: boolean
  schedule_enabled: boolean
  interval_minutes: number
}

/** The strip always renders this many slots; empty ones show as neutral bars. */
export const historySlots = 60

export const verdictLabel: Record<Verdict, string> = {
  passed: '满血',
  failed: '降智',
  unknown: '无法判定',
  error: '执行异常',
  pending: '检测中',
}

export const verdictTone: Record<Verdict, string> = {
  passed: 'ok',
  failed: 'bad',
  unknown: 'warn',
  error: 'muted',
  pending: 'brand',
}

export const phaseLabel: Record<string, string> = {
  queued: '排队中',
  generating: '生成 HTML',
  generated: '等待评价',
  assessing: '评价中',
  testing: '检测中',
  complete: '已完成',
}

export const benchmarkLabel: Record<string, string> = {
  candy: '糖果',
  drawing: '画图',
}

// judged reports whether a run carries a definite quality conclusion. Only
// these enter the strip and the pass rate.
export function judged(run: Run | undefined): boolean {
  return !!run && (run.verdict === 'passed' || run.verdict === 'failed')
}

// active reports whether a run can still change.
export function active(run: Run | undefined): boolean {
  return !!run && (run.status === 'queued' || run.status === 'running')
}

// observationTime prefers the completion time and falls back to the last update
// so a legacy record still shows a stable moment.
export function observationTime(run: Run | undefined): number {
  if (!run) return 0
  return run.finished_at || run.updated_at
}
