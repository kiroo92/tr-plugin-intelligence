<script setup lang="ts">
import { computed, ref } from 'vue'
import { observationTime, verdictLabel, type Run } from './types'

// HistoryStrip renders a fixed sixty-slot timeline. Empty slots keep sparse
// histories readable as a timeline instead of a short block of bars. Hover or
// keyboard focus opens one centered panel with the slot's full timestamp and
// judgement, mirroring the upstream result bar without any navigation action.

const props = defineProps<{ runs: Run[]; slots?: number }>()

const slotCount = computed(() => props.slots ?? 60)
// ordered lists the runs oldest first, matching the strip's left-to-right axis.
const ordered = computed(() => [...props.runs].slice(-slotCount.value))
const emptySlots = computed(() => Math.max(0, slotCount.value - ordered.value.length))

const hovered = ref<Run | null>(null)

// tone maps a verdict onto the slot class. Unknown and error states stay
// visually distinct from a failed answer.
function tone(run: Run): string {
  switch (run.verdict) {
    case 'passed': return 'ok'
    case 'failed': return 'bad'
    case 'unknown': return 'unknown'
    case 'error': return 'error'
    default: return ''
  }
}

// stamp formats the full local timestamp shown in the hover panel.
function stamp(seconds: number): string {
  const date = new Date(seconds * 1000)
  const pad = (value: number): string => String(value).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ` +
    `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`
}

// label describes one slot for assistive technology.
function label(run: Run): string {
  return `${stamp(observationTime(run))} ${verdictLabel[run.verdict]}`
}

// show opens the panel for one slot.
function show(run: Run): void {
  hovered.value = run
}

// hide closes the panel when the pointer leaves the whole strip, so moving
// between neighbouring slots does not flicker.
function hide(): void {
  hovered.value = null
}

// close on Escape for keyboard users.
function escape(event: KeyboardEvent): void {
  if (event.key === 'Escape') hide()
}
</script>

<template>
  <div class="in-history-wrap" @mouseleave="hide">
    <div class="in-strip" role="group" :aria-label="`最近 ${slotCount} 次明确结论`" @keydown="escape">
      <span v-for="slot in emptySlots" :key="`empty-${slot}`" class="in-slot" aria-hidden="true" />
      <button
        v-for="run in ordered"
        :key="run.id"
        type="button"
        class="in-slot"
        :class="tone(run)"
        :aria-label="label(run)"
        :title="label(run)"
        @mouseenter="show(run)"
        @focus="show(run)"
      />
    </div>
    <div v-if="hovered" class="in-hover" role="tooltip">
      <div class="in-hover-head">
        <strong>{{ stamp(observationTime(hovered)) }}</strong>
        <span class="in-pill" :class="tone(hovered)">{{ verdictLabel[hovered.verdict] }}</span>
      </div>
      <div class="in-hover-body">
        <span>{{ hovered.model }} · {{ hovered.benchmark === 'drawing' ? '画图' : '糖果' }}</span>
        <span v-if="hovered.assessment_reason">{{ hovered.assessment_reason }}</span>
        <span v-else-if="hovered.error_message">{{ hovered.error_message }}</span>
        <span v-if="hovered.duration_ms != null">耗时 {{ (hovered.duration_ms / 1000).toFixed(1) }} s</span>
      </div>
    </div>
  </div>
</template>
