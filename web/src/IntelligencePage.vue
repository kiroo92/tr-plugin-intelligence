<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { TrButton, useHost } from '@tokenflux/tokenrouter-plugin-sdk'
import HistoryStrip from './HistoryStrip.vue'
import {
  active as isActive, benchmarkLabel, judged, observationTime, phaseLabel, verdictLabel, verdictTone,
  type AdminList, type CatalogGroup, type ConfigInput, type ConfigView, type Run, type RunDetail,
  type Settings, type UserTests,
} from './types'

// The page follows the upstream layout: one card per routing group, a fixed
// model column beside each model's observation rows, and a sixty-slot result
// strip per benchmark. The administrator page adds configuration actions; the
// user page stays read-only.

const host = useHost()
const admin = computed(() => host.route.value.pageId === 'admin')

const list = ref<AdminList>()
const settings = ref<Settings>()
const tests = ref<UserTests>()
const busy = ref(false)
const failure = ref('')
const notice = ref('')

const controller = new AbortController()
let timer: ReturnType<typeof setInterval> | undefined

const dialog = reactive<{ kind: '' | 'config' | 'settings' | 'runs' | 'detail'; configId: number }>({
  kind: '', configId: 0,
})
const runs = ref<Run[]>([])
const form = ref<ConfigInput & { id: number; key_set: boolean }>(blankForm())
const detail = ref<Run>()
const documentSource = ref('')

// catalogue holds the caller's visible groups with their multipliers and the
// model identifiers each public group lists. It fills the configuration form.
const catalog = ref<CatalogGroup[]>([])
const catalogLoading = ref(false)
const catalogError = ref('')

// selectedModels lists the identifiers offered for the chosen group.
const selectedModels = computed(() => {
  const group = catalog.value.find(item => item.id === form.value.group_id)
  return group?.models ?? []
})

function blankForm(): ConfigInput & { id: number; key_set: boolean } {
  return {
    id: 0, key_set: false, group_id: 0, group_name: '', model: '', benchmark: 'candy',
    prompt: '', base_url: '', api_key: '', protocol: 'responses', reasoning_effort: '',
    service_tier: '', enabled: true, schedule_enabled: false, interval_minutes: 60,
  }
}

function errorText(error: unknown): string {
  return error instanceof Error ? error.message : '请求失败，请稍后重试'
}

async function run(action: () => Promise<void>): Promise<void> {
  if (busy.value) return
  busy.value = true
  failure.value = ''
  notice.value = ''
  try {
    await action()
  } catch (error) {
    if (!controller.signal.aborted) failure.value = errorText(error)
  } finally {
    busy.value = false
  }
}

async function refresh(): Promise<void> {
  if (admin.value) {
    const [next, current] = await Promise.all([
      host.request<AdminList>('GET', '/admin/configs', { signal: controller.signal }),
      host.request<Settings>('GET', '/admin/settings', { signal: controller.signal }),
    ])
    list.value = next
    settings.value = current
  } else {
    tests.value = await host.request<UserTests>('GET', '/tests', { signal: controller.signal })
  }
}

async function reload(): Promise<void> {
  await run(refresh)
}

/* ---------- grouping ---------- */

// ModelGroup collapses one model's benchmark configurations so the fixed model
// column is shared by their rows, as on the upstream page.
interface ModelGroup {
  model: string
  views: ConfigView[]
}

// GroupCard is one routing group with its ordered models.
interface GroupCard {
  id: number
  name: string
  rate: number
  models: ModelGroup[]
}

// groupConfigs builds the card structure from a flat list of configurations.
function groupViews(views: ConfigView[]): GroupCard[] {
  const groups = new Map<number, GroupCard>()
  for (const view of views) {
    let group = groups.get(view.config.group_id)
    if (!group) {
      group = {
        id: view.config.group_id,
        name: view.config.group_name,
        rate: view.rate_multiplier,
        models: [],
      }
      groups.set(view.config.group_id, group)
    }
    let model = group.models.find(item => item.model === view.config.model)
    if (!model) {
      model = { model: view.config.model, views: [] }
      group.models.push(model)
    }
    model.views.push(view)
  }
  // Drawing before candy keeps the artwork row first, matching upstream order.
  for (const group of groups.values()) {
    for (const model of group.models) {
      model.views.sort((left, right) => (left.config.benchmark === 'drawing' ? -1 : 1))
    }
  }
  return [...groups.values()]
}

const adminCards = computed(() => groupViews(list.value?.configs ?? []))
const userCards = computed(() => groupViews((tests.value?.groups ?? []).flatMap(group => group.configs)))

const metrics = computed(() => {
  const views = list.value?.configs ?? []
  return {
    total: views.length,
    scheduled: views.filter(view => view.config.schedule_enabled).length,
    active: views.filter(view => view.active).length,
    missing: views.filter(view => !view.config.key_set).length,
  }
})

const issues = computed(() => {
  const items: { tone: string; text: string; action: string; configId?: number }[] = []
  if (!admin.value) return items
  if (list.value && !list.value.master_key_set) {
    items.push({ tone: 'bad', text: '凭据主密钥未配置。保存 API Key 与运行检测都会被拒绝，请先在宿主「插件配置」填写。', action: 'settings' })
  }
  if (metrics.value.missing) {
    items.push({ tone: 'warn', text: `${metrics.value.missing} 个配置还没有可用 API Key，定时检测不会发起请求。`, action: '' })
  }
  for (const view of list.value?.configs ?? []) {
    const recent = view.history.slice(-3)
    if (recent.length === 3 && recent.every(entry => entry.verdict === 'failed')) {
      items.push({
        tone: 'bad',
        text: `${view.config.model} · ${benchmarkLabel[view.config.benchmark]} 连续 3 次判为降智。`,
        action: 'runs', configId: view.config.id,
      })
    }
  }
  return items
})

/* ---------- formatting ---------- */

function pad(value: number): string {
  return String(value).padStart(2, '0')
}

function clockText(seconds?: number): string {
  if (!seconds) return '—'
  const date = new Date(seconds * 1000)
  const today = new Date()
  const time = `${pad(date.getHours())}:${pad(date.getMinutes())}`
  if (date.toDateString() === today.toDateString()) return `今天 ${time}`
  return `${date.getMonth() + 1} 月 ${date.getDate()} 日 ${time}`
}

function fullText(seconds?: number): string {
  if (!seconds) return '—'
  const date = new Date(seconds * 1000)
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ` +
    `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`
}

function relativeText(seconds?: number): string {
  if (!seconds) return ''
  const minutes = Math.max(0, Math.round((Date.now() / 1000 - seconds) / 60))
  if (minutes < 1) return '刚刚'
  if (minutes < 60) return `${minutes} 分钟前`
  const hours = Math.round(minutes / 60)
  if (hours < 24) return `${hours} 小时前`
  return `${Math.round(hours / 24)} 天前`
}

function durationText(ms?: number): string {
  if (ms == null) return '—'
  if (ms < 1000) return `${ms} ms`
  if (ms < 60000) return `${(ms / 1000).toFixed(1)} s`
  return `${Math.floor(ms / 60000)} 分 ${Math.round((ms % 60000) / 1000)} 秒`
}

function tokenText(run: Run): string {
  if (run.input_tokens == null && run.output_tokens == null) return '—'
  return `${run.input_tokens ?? 0} / ${run.output_tokens ?? 0}`
}

function phaseText(run?: Run): string {
  return run ? phaseLabel[run.phase] ?? '检测中' : ''
}

// verdictOf reports the plate shown beside a row's last judged result.
function verdictOf(view: ConfigView): keyof typeof verdictLabel {
  return view.latest_judged?.verdict ?? 'unknown'
}

// countsText summarises the strip header, matching upstream wording.
function countsText(view: ConfigView): string {
  const passed = view.history.filter(entry => entry.verdict === 'passed').length
  const failed = view.history.filter(entry => entry.verdict === 'failed').length
  return `满血 ${passed} · 降智 ${failed}`
}

/* ---------- dialogs ---------- */

function show(id: string): void {
  requestAnimationFrame(() => {
    const element = document.getElementById(`intel-${id}`) as HTMLDialogElement | null
    if (element && !element.open) element.showModal()
  })
}

function closeDialog(): void {
  for (const id of ['config', 'settings', 'runs', 'detail']) {
    const element = document.getElementById(`intel-${id}`) as HTMLDialogElement | null
    if (element?.open) element.close()
  }
  dialog.kind = ''
  detail.value = undefined
  documentSource.value = ''
}

function openConfig(view?: ConfigView, groupId?: number): void {
  form.value = blankForm()
  if (view) {
    Object.assign(form.value, {
      id: view.config.id,
      key_set: view.config.key_set,
      group_id: view.config.group_id,
      group_name: view.config.group_name,
      model: view.config.model,
      benchmark: view.config.benchmark,
      prompt: view.config.prompt,
      base_url: view.config.base_url,
      api_key: '',
      protocol: view.config.protocol,
      reasoning_effort: view.config.reasoning_effort,
      service_tier: view.config.service_tier,
      enabled: view.config.enabled,
      schedule_enabled: view.config.schedule_enabled,
      interval_minutes: view.config.interval_minutes,
    })
  } else if (groupId) {
    const source = (list.value?.configs ?? []).find(item => item.config.group_id === groupId)
    if (source) {
      form.value.group_id = source.config.group_id
      form.value.group_name = source.config.group_name
      form.value.base_url = source.config.base_url
      form.value.protocol = source.config.protocol
    }
  } else if (list.value?.configs.length) {
    const source = list.value.configs[0]!.config
    form.value.group_id = source.group_id
    form.value.group_name = source.group_name
    form.value.base_url = source.base_url
    form.value.protocol = source.protocol
  }
  dialog.kind = 'config'
  dialog.configId = view?.config.id ?? 0
  void loadCatalog()
  show('config')
}

// loadCatalog reads the visible groups and their model identifiers. A failure
// leaves the form usable through manual entry, with the reason shown inline.
async function loadCatalog(): Promise<void> {
  if (catalog.value.length) return
  catalogLoading.value = true
  catalogError.value = ''
  try {
    const response = await host.request<{ groups: CatalogGroup[] }>('GET', '/admin/groups', {
      signal: controller.signal,
    })
    catalog.value = response.groups ?? []
  } catch (error) {
    catalogError.value = '无法读取分组列表，请手动填写编号与名称。'
    if (!controller.signal.aborted) failure.value = errorText(error)
  } finally {
    catalogLoading.value = false
  }
}

// selectGroup applies the chosen group's identity and clears an incompatible
// model so a stale identifier cannot be saved against another group.
function selectGroup(value: string): void {
  const id = Number(value)
  const group = catalog.value.find(item => item.id === id)
  if (!group) return
  if (form.value.group_id !== id) form.value.model = ''
  form.value.group_id = group.id
  form.value.group_name = group.name
}

function useCurrentOrigin(): void {
  form.value.base_url = `${window.location.origin}/v1`
}

async function saveConfig(): Promise<void> {
  await run(async () => {
    const body: ConfigInput = {
      group_id: form.value.group_id,
      group_name: form.value.group_name,
      model: form.value.model,
      benchmark: form.value.benchmark,
      prompt: form.value.prompt,
      base_url: form.value.base_url,
      api_key: form.value.api_key,
      protocol: form.value.protocol,
      reasoning_effort: form.value.reasoning_effort,
      service_tier: form.value.service_tier,
      enabled: form.value.enabled,
      schedule_enabled: form.value.schedule_enabled,
      interval_minutes: Number(form.value.interval_minutes) || 60,
    }
    if (form.value.id) {
      await host.request('PUT', `/admin/configs/${form.value.id}`, { body, signal: controller.signal })
    } else {
      await host.request('POST', '/admin/configs', { body, signal: controller.signal })
    }
    closeDialog()
    await refresh()
    notice.value = '配置已保存'
  })
}

async function runNow(id: number): Promise<void> {
  await run(async () => {
    await host.request('POST', `/admin/actions/${id}/run`, { signal: controller.signal })
    await refresh()
    notice.value = '已加入检测队列'
  })
}

async function removeConfig(id: number): Promise<void> {
  const accepted = await host.confirm({
    title: '删除检测配置？',
    message: '该配置的历史结论与作品会一并删除，无法恢复。在途任务结束前不能删除。',
    danger: true,
  })
  if (!accepted) return
  await run(async () => {
    await host.request('DELETE', `/admin/configs/${id}`, { signal: controller.signal })
    closeDialog()
    await refresh()
    notice.value = '配置已删除'
  })
}

function openHistory(view: ConfigView): void {
  runs.value = [...view.history].reverse()
  dialog.configId = view.config.id
  dialog.kind = 'runs'
  show('runs')
}

async function openRun(id: string): Promise<void> {
  await run(async () => {
    const path = admin.value ? `/admin/runs/${id}` : `/runs/${id}`
    const response = await host.request<RunDetail>('GET', path, { signal: controller.signal })
    detail.value = response.run
    documentSource.value = response.html ?? ''
    dialog.kind = 'detail'
    show('detail')
  })
}

async function copySource(): Promise<void> {
  if (!documentSource.value) return
  try {
    await navigator.clipboard.writeText(documentSource.value)
    notice.value = '作品 HTML 已复制'
  } catch {
    failure.value = '无法自动复制，请手动选中源码复制'
  }
}

function downloadSource(): void {
  if (!documentSource.value) return
  const blob = new Blob([documentSource.value], { type: 'text/html;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = `drawing-${detail.value?.id ?? 'run'}.html`
  anchor.click()
  URL.revokeObjectURL(url)
}

function openSettings(): void {
  dialog.kind = 'settings'
  show('settings')
}

async function saveSettings(): Promise<void> {
  await run(async () => {
    settings.value = await host.request<Settings>('PUT', '/admin/settings', {
      body: { enabled: settings.value?.enabled ?? false },
      signal: controller.signal,
    })
    closeDialog()
    await refresh()
    notice.value = '设置已保存'
  })
}

function setEnabled(value: boolean): void {
  if (settings.value) settings.value.enabled = value
}

const canSave = computed(() => {
  if (!form.value.model.trim() || !form.value.base_url.trim() || form.value.group_id <= 0) return false
  if (!form.value.id && !form.value.api_key.trim()) return false
  return true
})

watch(admin, () => {
  list.value = undefined
  tests.value = undefined
  void reload()
})

onMounted(() => {
  void reload()
  timer = setInterval(() => {
    if (!busy.value && document.visibilityState === 'visible') void refresh().catch(() => undefined)
  }, 10000)
})

onBeforeUnmount(() => {
  controller.abort()
  clearInterval(timer)
})
</script>

<template>
<div class="intel">
  <header class="in-head">
    <div>
      <h1>{{ admin ? '智商测试' : '智商测试' }}</h1>
      <p>{{ admin
        ? '按分组配置模型与测试。画图测试在本站生成 HTML 后提交评价，可自定义生成提示词。'
        : '按分组和模型查看近期测试结果与绘图作品。测试结果仅反映当次测试表现。' }}</p>
    </div>
    <div class="in-head-actions">
      <TrButton :loading="busy" @click="reload">刷新</TrButton>
      <template v-if="admin">
        <TrButton @click="openSettings">系统设置</TrButton>
        <TrButton variant="primary" @click="openConfig()">新增测试</TrButton>
      </template>
    </div>
  </header>

  <p v-if="failure" class="in-banner bad" role="alert">{{ failure }}</p>
  <p v-if="notice" class="in-banner info" role="status">{{ notice }}</p>

  <div v-for="(issue, index) in issues" :key="index" class="in-banner" :class="issue.tone">
    <span>{{ issue.text }}</span>
    <TrButton v-if="issue.action === 'settings'" class="in-banner-action" @click="openSettings">查看设置</TrButton>
    <TrButton
      v-else-if="issue.action === 'runs' && issue.configId"
      class="in-banner-action"
      @click="openHistory((list?.configs ?? []).find(item => item.config.id === issue.configId)!)"
    >查看历史</TrButton>
  </div>

  <p v-if="admin && list && !list.enabled" class="in-banner info">
    <span><b>智商测试已关闭。</b>用户页不可访问，定时检测停止入队；已提交的远端任务继续收敛，仍可维护配置与查看文本历史。</span>
  </p>

  <div v-if="admin" class="in-metrics">
    <span>配置 <b>{{ metrics.total }}</b></span><i />
    <span>定时开启 <b>{{ metrics.scheduled }}</b></span><i />
    <span>在途任务 <b>{{ metrics.active }}</b></span><i />
    <span>缺少凭据 <b>{{ metrics.missing }}</b></span>
  </div>

  <!-- Legend, matching the upstream colour key -->
  <div v-if="(admin ? adminCards : userCards).length" class="in-legend" style="margin-bottom:10px">
    <span><i class="in-swatch" :style="{ background: 'var(--in-ok)' }" />满血</span>
    <span><i class="in-swatch" :style="{ background: 'var(--in-bad)' }" />降智</span>
    <span><i class="in-swatch" :style="{ background: 'var(--in-sub)', opacity: 0.55 }" />执行异常</span>
    <span style="margin-left:auto">每格表示一次明确结论，由旧到新</span>
  </div>

  <!-- Administrator cards -->
  <template v-if="admin">
    <div v-if="!adminCards.length" class="in-card">
      <div class="in-empty">
        <b>还没有测试配置</b>
        选择分组与模型，为它添加糖果测试或画图测试。
      </div>
    </div>
    <section v-for="card in adminCards" :key="card.id" class="in-card">
      <div class="in-card-head">
        <h2>{{ card.name }}</h2>
        <span class="in-rate">倍率 <b>{{ card.rate }}×</b></span>
        <span class="in-card-meta">{{ card.models.length }} 个模型 · {{ card.models.reduce((sum, model) => sum + model.views.length, 0) }} 个配置</span>
        <div class="in-card-actions">
          <TrButton @click="openConfig(undefined, card.id)">新增测试</TrButton>
        </div>
      </div>
      <div v-for="model in card.models" :key="model.model" class="in-model">
        <div class="in-model-id">
          <span class="in-model-name">{{ model.model }}</span>
          <span class="in-model-kind">{{ model.views.length > 1 ? '多类型' : benchmarkLabel[model.views[0]!.config.benchmark] }}</span>
        </div>
        <div class="in-obs">
          <article
            v-for="view in model.views"
            :key="view.config.id"
            class="in-obs-body"
            :class="{ 'is-active': view.active }"
            :data-config-id="view.config.id"
          >
            <!-- artwork cell -->
            <div class="in-art" :class="{ empty: !view.latest_judged?.has_html }">
              <button
                v-if="view.latest_judged?.has_html"
                type="button"
                :aria-label="`查看 ${model.model} 的作品`"
                @click="openRun(view.latest_judged!.id)"
              ><span class="in-art-tag">HTML</span></button>
              <span v-else class="in-sub">无作品</span>
            </div>
            <!-- last verdict + active phase -->
            <div class="in-verdict">
              <p class="in-verdict-label">最近结论</p>
              <span class="in-pill" :class="verdictTone[verdictOf(view)]">
                {{ view.latest_judged ? verdictLabel[verdictOf(view)] : '待检测' }}
              </span>
              <p v-if="view.active" class="in-phase"><span class="in-spin" />{{ phaseText(view.latest) }}</p>
              <p v-else-if="view.latest?.verdict === 'error'" class="in-phase in-danger">
                {{ view.latest.error_code === 'credential' ? '缺少 Key' : '上次异常' }}
              </p>
            </div>
            <!-- completion time -->
            <div class="in-time">
              <time v-if="view.latest_judged" :title="fullText(observationTime(view.latest_judged))">
                {{ clockText(observationTime(view.latest_judged)) }}
              </time>
              <span v-if="view.latest_judged">{{ relativeText(observationTime(view.latest_judged)) }}</span>
              <span v-else class="in-sub">尚无记录</span>
            </div>
            <!-- history strip -->
            <div>
              <div class="in-history-head">
                <span>明确结论 {{ view.history.length }} 次</span>
                <span>{{ countsText(view) }}</span>
              </div>
              <HistoryStrip :runs="view.history" />
              <div class="in-history-axis"><span>较早</span><span>最近</span></div>
            </div>
            <!-- actions -->
            <div class="in-row-actions">
              <TrButton
                :disabled="busy || view.active || !view.config.key_set || !view.config.enabled"
                @click="runNow(view.config.id)"
              >立即测试</TrButton>
              <TrButton @click="openHistory(view)">测试记录</TrButton>
              <TrButton @click="openConfig(view)">编辑</TrButton>
            </div>
            <div class="in-config-meta" style="grid-column:1/-1;margin-top:0">
              <span>{{ benchmarkLabel[view.config.benchmark] }}</span>
              <span>{{ view.config.protocol === 'responses' ? 'Responses' : 'Chat Completions' }}</span>
              <span>{{ view.config.schedule_enabled ? `每 ${view.config.interval_minutes} 分钟` : '仅手动' }}</span>
              <span :class="{ 'in-danger': !view.config.key_set }">{{ view.config.key_set ? 'Key 已配置' : '缺少 Key' }}</span>
              <span v-if="!view.config.enabled">已停用</span>
            </div>
          </article>
        </div>
      </div>
    </section>
  </template>

  <!-- User cards -->
  <template v-else>
    <div v-if="!userCards.length && tests" class="in-card">
      <div class="in-empty"><b>暂无测试结果</b>管理员配置并完成测试后，会在这里显示结果。</div>
    </div>
    <section v-for="card in userCards" :key="card.id" class="in-card">
      <div class="in-card-head">
        <h2>{{ card.name }}</h2>
        <span class="in-rate">倍率 <b>{{ card.rate }}×</b></span>
      </div>
      <div v-for="model in card.models" :key="model.model" class="in-model">
        <div class="in-model-id">
          <span class="in-model-name">{{ model.model }}</span>
          <span class="in-model-kind">{{ model.views.length > 1 ? '多类型' : benchmarkLabel[model.views[0]!.config.benchmark] }}</span>
        </div>
        <div class="in-obs">
          <article
            v-for="view in model.views"
            :key="view.config.id"
            class="in-obs-body"
            :class="{ 'is-active': view.active }"
          >
            <div class="in-art" :class="{ empty: !view.latest_judged?.has_html }">
              <button
                v-if="view.latest_judged?.has_html"
                type="button"
                :aria-label="`查看 ${model.model} 的作品`"
                @click="openRun(view.latest_judged!.id)"
              ><span class="in-art-tag">HTML</span></button>
              <span v-else class="in-sub">无作品</span>
            </div>
            <div class="in-verdict">
              <p class="in-verdict-label">最近有效结果</p>
              <span class="in-pill" :class="verdictTone[verdictOf(view)]">
                {{ view.latest_judged ? verdictLabel[verdictOf(view)] : '尚无结论' }}
              </span>
              <p v-if="view.active" class="in-phase"><span class="in-spin" />{{ phaseText(view.latest) }}</p>
            </div>
            <div class="in-time">
              <time v-if="view.latest_judged" :title="fullText(observationTime(view.latest_judged))">
                {{ clockText(observationTime(view.latest_judged)) }}
              </time>
              <span v-if="view.latest_judged">{{ relativeText(observationTime(view.latest_judged)) }}</span>
              <span v-else class="in-sub">等待首次检测</span>
            </div>
            <div>
              <div class="in-history-head">
                <span>有效结果 {{ view.history.length }} 次</span>
                <span>{{ countsText(view) }}</span>
              </div>
              <HistoryStrip :runs="view.history" />
              <div class="in-history-axis"><span>较早</span><span>最近</span></div>
            </div>
            <div />
          </article>
        </div>
      </div>
    </section>
  </template>

  <!-- Configuration dialog -->
  <dialog v-if="dialog.kind === 'config'" id="intel-config" class="in-dialog" @close="dialog.kind = ''">
    <form method="dialog" @submit.prevent="saveConfig">
      <div class="in-dialog-head">
        <div>
          <h2>{{ form.id ? '编辑测试' : '新增测试' }}</h2>
          <p>分组、模型与测试类型构成固定身份，创建后不可更改；需要换组合请新建。</p>
        </div>
        <TrButton @click="closeDialog">关闭</TrButton>
      </div>
      <div class="in-dialog-body">
        <div class="in-two">
          <label class="in-field">
            <span>分组 <em v-if="!form.id && catalogLoading" style="margin:0">读取中…</em></span>
            <select :value="form.group_id" :disabled="!!form.id || catalogLoading" required @change="selectGroup(($event.target as HTMLSelectElement).value)">
              <option :value="0" disabled>请选择分组</option>
              <option v-for="group in catalog" :key="group.id" :value="group.id">
                {{ group.name }} · {{ group.rate_multiplier }}×（#{{ group.id }}）
              </option>
              <option v-if="form.id && !catalog.some(group => group.id === form.group_id)" :value="form.group_id">
                {{ form.group_name }}（#{{ form.group_id }}）
              </option>
            </select>
            <em>{{ catalogError || '来自宿主的分组授权；倍率是当前账号的实际计费倍率。' }}</em>
          </label>
          <label class="in-field">
            <span>测试类型</span>
            <select v-model="form.benchmark" :disabled="!!form.id">
              <option value="candy">糖果题</option>
              <option value="drawing">画图</option>
            </select>
            <em>{{ form.benchmark === 'candy' ? '抽屉原理应用题，答案 21；仅 Responses 协议。' : '生成单文件 HTML 动画，再由评价服务判定。' }}</em>
          </label>
        </div>
        <label class="in-field">
          <span>模型标识 <em v-if="!form.id && selectedModels.length" style="margin:0">该分组 {{ selectedModels.length }} 个可选</em></span>
          <input
            v-model="form.model"
            list="intel-model-options"
            maxlength="256"
            :disabled="!!form.id"
            placeholder="选择或直接填写模型标识"
            required
          >
          <datalist id="intel-model-options">
            <option v-for="model in selectedModels" :key="model.id" :value="model.id">{{ model.name }}</option>
          </datalist>
          <div v-if="!form.id && selectedModels.length" class="in-model-options">
            <button
              v-for="model in selectedModels"
              :key="model.id"
              type="button"
              class="in-model-option"
              :class="{ selected: form.model === model.id }"
              :title="model.name"
              @click="form.model = model.id"
            >{{ model.id }}</button>
          </div>
          <em v-if="form.group_id && !selectedModels.length">
            该分组没有公开模型可选项（仅公开分组会出现在模型广场），请直接填写与网关请求一致的 model。
          </em>
          <em v-else>与网关请求中的 model 完全一致；列表来自模型广场的公开分组。</em>
        </label>
        <label class="in-field">
          <span>Base URL <TrButton @click="useCurrentOrigin">使用当前网站</TrButton></span>
          <input v-model="form.base_url" type="url" maxlength="2048" placeholder="https://your-site/v1" required>
          <em>仅公网 HTTPS；拒绝账号密码、查询参数、片段与私网地址。{{ form.benchmark === 'candy' ? '糖果测试会把此地址与 Key 发送给评价服务。' : '画图由插件后端访问该地址。' }}</em>
        </label>
        <label class="in-field">
          <span>API Key <em v-if="form.id" style="margin:0">{{ form.key_set ? '已配置，留空保留' : '尚未配置' }}</em></span>
          <input v-model="form.api_key" type="password" autocomplete="new-password" :placeholder="form.id && form.key_set ? '已配置，输入新值可替换' : 'sk-...'">
          <em class="in-warn">手动填写。插件无法校验 Key 归属与额度，填错只会得到上游 401/403；建议使用专用、可撤销的 Key。</em>
        </label>
        <div class="in-two">
          <label class="in-field">
            <span>请求协议</span>
            <select v-model="form.protocol" :disabled="form.benchmark === 'candy'">
              <option value="responses">Responses</option>
              <option value="chat_completions">Chat Completions</option>
            </select>
            <em v-if="form.benchmark === 'candy'">糖果测试固定使用 Responses。</em>
          </label>
          <label class="in-field">
            <span>推理强度</span>
            <select v-model="form.reasoning_effort">
              <option value="">默认</option>
              <option value="low">low</option>
              <option value="medium">medium</option>
              <option value="high">high</option>
              <template v-if="form.benchmark === 'candy'">
                <option value="xhigh">xhigh</option>
                <option value="max">max</option>
                <option value="ultra">ultra</option>
              </template>
            </select>
          </label>
        </div>
        <label class="in-field">
          <span>服务档位</span>
          <select v-model="form.service_tier">
            <option value="">默认</option>
            <option value="priority">priority</option>
            <option value="ultrafast">ultrafast</option>
          </select>
        </label>
        <label v-if="form.benchmark === 'drawing'" class="in-field">
          <span>画图提示词 <em style="margin:0">最多 32 KiB</em></span>
          <textarea v-model="form.prompt" maxlength="32768" placeholder="留空使用内置提示词（鹈鹕骑自行车动画）" />
        </label>
        <div class="in-section">
          <h3>定时</h3>
          <label class="in-check"><input v-model="form.schedule_enabled" type="checkbox">按间隔自动检测</label>
          <label class="in-field">
            <span>间隔（分钟）<em style="margin:0">5–10080</em></span>
            <input v-model.number="form.interval_minutes" type="number" min="5" max="10080">
            <em>{{ form.schedule_enabled ? `每 ${form.interval_minutes} 分钟自动检测` : '仅手动触发' }}</em>
          </label>
        </div>
        <label class="in-check"><input v-model="form.enabled" type="checkbox">启用此配置</label>
      </div>
      <div class="in-dialog-foot">
        <span class="in-sub">{{ form.id ? '执行参数与凭据的修改要求在途任务结束后进行。' : '保存后不会立即发请求，可先手动测试验证。' }}</span>
        <div class="in-grow">
          <TrButton v-if="form.id" @click="removeConfig(form.id)">删除</TrButton>
          <TrButton @click="closeDialog">取消</TrButton>
          <TrButton variant="primary" :loading="busy" :disabled="!canSave" @click="saveConfig">保存配置</TrButton>
        </div>
      </div>
    </form>
  </dialog>

  <!-- Settings dialog -->
  <dialog v-if="dialog.kind === 'settings'" id="intel-settings" class="in-dialog" @close="dialog.kind = ''">
    <div class="in-dialog-head">
      <div><h2>系统设置</h2><p>插件级开关与凭据保护状态。</p></div>
      <TrButton @click="closeDialog">关闭</TrButton>
    </div>
    <div class="in-dialog-body">
      <label class="in-check">
        <input type="checkbox" :checked="settings?.enabled" @change="setEnabled(($event.target as HTMLInputElement).checked)">
        开启智商测试
      </label>
      <p class="in-sub">关闭后隐藏用户页入口、停止新的定时检测；已在途的远端任务继续收敛，管理员仍可维护配置与查看文本历史。</p>
      <div class="in-section">
        <h3>凭据主密钥</h3>
        <p v-if="settings?.master_key_set" class="in-sub">已配置。各配置的 API Key 使用 AES-256-GCM 加密后存入插件数据库，解密只发生在发起请求前。</p>
        <p v-else class="in-banner bad" style="margin:0">未配置。保存 Key 与运行检测都会被拒绝，插件不会回退到明文存储。请在宿主「插件配置」填写至少 32 个字符的主密钥。</p>
      </div>
      <div class="in-section">
        <h3>默认值</h3>
        <div class="in-kv">
          <div><dt>默认间隔</dt><dd>60 分钟</dd></div>
          <div><dt>配置上限</dt><dd>200 份</dd></div>
          <div><dt>历史保留</dt><dd>60 条</dd></div>
          <div><dt>HTML 保留</dt><dd>10 份</dd></div>
        </div>
      </div>
    </div>
    <div class="in-dialog-foot">
      <span class="in-sub">主密钥更换前需停止任务并重填凭据。</span>
      <div class="in-grow"><TrButton variant="primary" :loading="busy" @click="saveSettings">保存</TrButton></div>
    </div>
  </dialog>

  <!-- History dialog -->
  <dialog v-if="dialog.kind === 'runs'" id="intel-runs" class="in-dialog wide" @close="dialog.kind = ''">
    <div class="in-dialog-head">
      <div><h2>测试记录</h2><p>由新到旧；执行异常与未知结论单独标注，不计入通过率。</p></div>
      <TrButton @click="closeDialog">关闭</TrButton>
    </div>
    <div class="in-dialog-body">
      <div v-if="!runs.length" class="in-empty">暂无测试记录。</div>
      <div v-else class="in-runs">
        <div v-for="entry in runs" :key="entry.id" class="in-run">
          <span class="in-pill" :class="verdictTone[entry.verdict]">{{ verdictLabel[entry.verdict] }}</span>
          <div>
            <b>{{ entry.verdict === 'error' ? (entry.error_message || '执行异常') : entry.verdict === 'unknown' ? (entry.error_message || '评价缺失或无法判定') : (entry.assessment_reason || '判定正常') }}</b>
            <span>{{ fullText(observationTime(entry)) }} · {{ durationText(entry.duration_ms) }}</span>
          </div>
          <div class="in-run-stat">
            <template v-if="judged(entry)">首字 {{ durationText(entry.first_token_ms) }}<br>{{ tokenText(entry) }} tokens</template>
            <template v-else>{{ phaseLabel[entry.phase] ?? '—' }}</template>
          </div>
        </div>
      </div>
    </div>
  </dialog>

  <!-- Detail dialog -->
  <dialog v-if="dialog.kind === 'detail'" id="intel-detail" class="in-dialog wide" @close="dialog.kind = ''">
    <div class="in-dialog-head">
      <div><h2>检测详情</h2><p>{{ detail ? `${detail.model} · ${benchmarkLabel[detail.benchmark]}` : '' }}</p></div>
      <TrButton @click="closeDialog">关闭</TrButton>
    </div>
    <div v-if="detail" class="in-dialog-body">
      <div class="in-kv">
        <div><dt>结论</dt><dd>{{ verdictLabel[detail.verdict] }}</dd></div>
        <div><dt>完成时间</dt><dd>{{ clockText(observationTime(detail)) }}</dd></div>
        <div><dt>耗时</dt><dd>{{ durationText(detail.duration_ms) }}</dd></div>
        <div><dt>Token</dt><dd>{{ tokenText(detail) }}</dd></div>
      </div>
      <p v-if="detail.error_message" class="in-banner bad" style="margin:12px 0 0">{{ detail.error_message }}</p>
      <div v-if="detail.assessment_reason" class="in-section">
        <h3>评价说明</h3>
        <p class="in-prompt">{{ detail.assessment_reason }}</p>
      </div>
      <div v-if="detail.answer" class="in-section">
        <h3>模型回答</h3>
        <p class="in-prompt">{{ detail.answer }}</p>
      </div>
      <div v-if="detail.question" class="in-section">
        <h3>题目</h3>
        <p class="in-prompt">{{ detail.question }}</p>
      </div>
      <div v-if="documentSource" class="in-section">
        <h3>作品</h3>
        <p class="in-sub">模型返回的 HTML 是不可信内容，插件不内嵌渲染。复制或下载后，请在隔离环境中打开。</p>
        <p class="in-prompt" style="margin-top:8px">{{ documentSource }}</p>
      </div>
      <div v-else-if="detail && isActive(detail) === false" class="in-section">
        <p class="in-sub">该次检测没有保留作品，或作品已被保留规则清理。</p>
      </div>
    </div>
    <div class="in-dialog-foot">
      <span class="in-sub">响应不包含 Key 或远端任务编号。</span>
      <div class="in-grow">
        <TrButton v-if="documentSource" @click="copySource">复制 HTML</TrButton>
        <TrButton v-if="documentSource" @click="downloadSource">下载 HTML</TrButton>
      </div>
    </div>
  </dialog>
</div>
</template>
