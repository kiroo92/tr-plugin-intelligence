<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'

// HtmlPreview.vue renders a saved model document inside an opaque sandbox.
// The document arrives through the authenticated JSON API and is parsed in a
// detached document before iframe navigation. Scripts can draw and animate,
// while the sandbox denies access to the parent page, storage, popups, forms,
// downloads, and top-level navigation. Compact list previews scale the desktop
// canvas into their buttons. Removing the containing dialog or list card stops
// the frame and its running scripts.
//
// A srcdoc frame inherits the host's content policy, including its script nonce.
// Inline script elements receive that nonce, and a leading additional policy
// restricts the document to inline code and embedded media. External resources,
// child frames, workers, and API connections are denied by that additional
// policy. Model-supplied policies and refresh directives are removed before
// serialization so the preview controls its initial document policy.

// Saved markup and the accessible frame label belong to the selected detail;
// changing the markup replaces the complete isolated document and its script state.
const props = defineProps<{
  html: string // saved model document
  title: string // accessible artwork label
  // compact scales the desktop drawing into its parent thumbnail. The iframe
  // leaves keyboard navigation to the containing preview button in this mode.
  compact?: boolean
}>()
const source = computed(() => previewDocument(props.html))
// Compact measurements belong to this mounted frame; its parent's dimensions
// determine the scale while the document keeps a stable desktop canvas size.
const frame = ref<HTMLIFrameElement>()
const scale = ref(1)
const frameStyle = computed(() => ({
  width: '1280px',
  height: '960px',
  minHeight: '0',
  flexShrink: 0,
  border: '0',
  borderRadius: '0',
  transform: `scale(${scale.value})`,
}))
let sizeObserver: ResizeObserver | undefined

// resize fits the complete desktop canvas into its containing thumbnail.
// Both dimensions constrain the scale, preserving the entire artwork when
// responsive cards change size while their saved HTML keeps fixed dimensions.
function resize(): void {
  const parent = frame.value?.parentElement
  if (!parent) return
  // Use a stable desktop viewport so fixed-size model documents are scaled
  // together with their backgrounds and animation instead of being cropped by cards.
  const next = Math.min(parent.clientWidth / 1280, parent.clientHeight / 960)
  if (next > 0) scale.value = next
}

onMounted(() => {
  if (!props.compact) return
  resize()
  if (typeof ResizeObserver !== 'undefined' && frame.value?.parentElement) {
    sizeObserver = new ResizeObserver(resize)
    sizeObserver.observe(frame.value.parentElement)
  } else {
    window.addEventListener('resize', resize)
  }
})

onBeforeUnmount(() => {
  sizeObserver?.disconnect()
  window.removeEventListener('resize', resize)
})

// previewDocument prepares saved HTML for a frame with sandbox="allow-scripts".
// Callers keep that sandbox and omit allow-same-origin because scripts are
// intentionally executable. The host's nonce authorizes inline script elements
// under the inherited policy; an additional leading policy limits their resources.
//
// Parsing remains detached from the application DOM, and supplied base URLs and
// HTTP-equivalent metadata are removed. The returned string belongs exclusively
// in srcdoc; inserting it into the host document would invalidate this isolation.
function previewDocument(html: string): string {
  const parsed = new DOMParser().parseFromString(html, 'text/html')
  const nonce = document.querySelector<HTMLScriptElement>('script[nonce]')?.nonce ?? ''
  // Discard refresh directives and competing policies before placing our policy
  // first, so model markup cannot navigate during parsing or alter resource permissions.
  for (const element of parsed.querySelectorAll('base, meta[http-equiv]')) element.remove()
  const policy = parsed.createElement('meta')
  policy.httpEquiv = 'Content-Security-Policy'
  policy.content = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; " +
    "img-src data: blob:; font-src data:; media-src data: blob:; " +
    "connect-src 'none'; frame-src 'none'; worker-src 'none'; " +
    "object-src 'none'; base-uri 'none'; form-action 'none'"
  parsed.head.prepend(policy)
  // Authorize inline elements under the inherited host policy. The additional
  // policy still denies external script sources even when those elements carry a nonce.
  for (const script of parsed.querySelectorAll('script')) {
    if (nonce) script.setAttribute('nonce', nonce)
    else script.removeAttribute('nonce')
  }
  return '<!doctype html>\n' + parsed.documentElement.outerHTML
}
</script>

<template>
  <iframe
    ref="frame"
    class="in-html-preview"
    :srcdoc="source"
    :title="title"
    :style="compact ? frameStyle : undefined"
    :tabindex="compact ? -1 : undefined"
    sandbox="allow-scripts"
    referrerpolicy="no-referrer"
    allow="camera 'none'; microphone 'none'; geolocation 'none'; clipboard-read 'none'; clipboard-write 'none'"
  />
</template>
