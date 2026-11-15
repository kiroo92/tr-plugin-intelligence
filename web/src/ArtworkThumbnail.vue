<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useHost } from '@tokenflux/tokenrouter-plugin-sdk'
import HtmlPreview from './HtmlPreview.vue'
import type { RunDetail } from './types'

// ArtworkThumbnail.vue displays retained artwork inside a list's preview button.
// It reads the document through the current audience's authenticated detail
// route, then uses the same opaque sandbox as the full preview. A desktop
// canvas is scaled into the thumbnail so fixed-size drawings remain visible.
// The containing button owns navigation and its accessible label.
//
// Documents load near the viewport and are released when the card or page
// becomes hidden. Each visibility or identity change cancels the preceding
// read and advances a revision, preventing late responses from displaying a
// different run's artwork. Teardown disconnects observation, removes the page
// visibility listener, and cancels any remaining request before the frame is
// removed with its owner.

// The selected observation and frame label follow the enclosing row. A changed
// run identifier replaces the thumbnail document through a fresh authorized read.
const props = defineProps<{
  runId: string // retained observation identifier
  title: string // accessible artwork label
}>()
// Visibility, retained markup, and request generations belong to this card;
// its page listener and intersection observer are released together during teardown.
const host = useHost()
const container = ref<HTMLElement>()
const visible = ref(false)
const pageVisible = ref(!document.hidden)
const source = ref('')
const unavailable = ref(false)
let observer: IntersectionObserver | undefined
let controller: AbortController | undefined
let revision = 0

// load replaces the visible thumbnail with its selected observation's document.
// Hidden cards release their content and pending requests. The revision rejects
// a preceding read even when its transport completes after cancellation.
//
// Request scope follows the host page, so administrator thumbnails and user
// thumbnails use their respective protected endpoints. A failed or empty read
// displays a compact unavailable state while the containing button can still
// open the observation's diagnostic detail.
async function load(): Promise<void> {
  const current = ++revision
  // Invalidate the previous request before clearing its document so a late
  // completion cannot repopulate a card that has scrolled away or changed identity.
  controller?.abort()
  source.value = ''
  unavailable.value = false
  if (!visible.value || !pageVisible.value) return
  const request = new AbortController()
  controller = request
  try {
    const prefix = host.route.value.pageId === 'admin' ? '/admin' : ''
    const result = await host.request<RunDetail>('GET', `${prefix}/runs/${encodeURIComponent(props.runId)}`, {
      signal: request.signal,
    })
    if (current !== revision || request.signal.aborted) return
    source.value = result.html ?? ''
    unavailable.value = !source.value
  } catch {
    // Cancellation belongs to visibility and selection cleanup. Show unavailable
    // only for a failed current read so normal scrolling does not flash an error.
    if (current === revision && !request.signal.aborted) unavailable.value = true
  }
}

// visibilityChanged updates whether the page can keep artwork scripts running.
function visibilityChanged(): void {
  pageVisible.value = !document.hidden
}

watch([() => props.runId, () => host.route.value.pageId, visible, pageVisible], () => void load())

onMounted(() => {
  document.addEventListener('visibilitychange', visibilityChanged)
  if (typeof IntersectionObserver === 'undefined') {
    visible.value = true
    return
  }
  observer = new IntersectionObserver((entries) => {
    visible.value = entries.some(entry => entry.isIntersecting)
  }, { rootMargin: '80px' })
  if (container.value) observer.observe(container.value)
})

onBeforeUnmount(() => {
  ++revision
  controller?.abort()
  observer?.disconnect()
  document.removeEventListener('visibilitychange', visibilityChanged)
})
</script>

<template>
  <div ref="container" class="in-artwork-thumbnail">
    <HtmlPreview v-if="source" :html="source" :title="title" compact />
    <span v-else class="in-sub" :role="unavailable ? undefined : 'status'">
      {{ unavailable ? '预览不可用' : '加载预览…' }}
    </span>
  </div>
</template>
