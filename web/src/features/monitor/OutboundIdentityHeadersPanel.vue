<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

import CopyButton from '@/components/ui/CopyButton.vue'
import AppTooltip from '@/components/ui/AppTooltip.vue'
import type { OutboundIdentityHeadersDto } from '@/app/resources/request-logs'
import { outboundHeaderRows } from './outbound-headers'

const props = defineProps<{
  headers: OutboundIdentityHeadersDto | null
}>()
const { t } = useI18n()
const rows = computed(() => (props.headers ? outboundHeaderRows(props.headers) : []))
</script>

<template>
  <div class="log-outbound-headers">
    <p class="log-outbound-headers__label">{{ t('monitor.logs.drawer.outboundHeaders') }}</p>
    <div v-if="rows.length > 0" class="log-outbound-headers__list">
      <div v-for="row in rows" :key="row.name" class="log-outbound-headers__row">
        <code class="log-outbound-headers__name">{{ row.name }}</code>
        <div class="log-outbound-headers__value">
          <code v-if="row.value" class="log-outbound-headers__value-code">{{ row.value }}</code>
          <span v-else class="log-outbound-headers__empty">—</span>
          <CopyButton
            v-if="row.value"
            :value="row.value"
            :label="t('monitor.logs.drawer.copyOutboundHeader')"
            :success-label="t('common.copied')"
            :failure-label="t('common.copyFailed')"
          />
          <AppTooltip
            v-if="row.decoded"
            :content="row.decoded"
            side="bottom"
            align="start"
            :disabled="!row.decoded"
          >
            <button
              class="log-outbound-headers__decode"
              type="button"
              tabindex="0"
              :aria-label="t('monitor.logs.drawer.decodeOutboundHeader')"
            >
              {{ t('monitor.logs.drawer.decodeOutboundHeader') }}
            </button>
          </AppTooltip>
        </div>
      </div>
    </div>
    <p v-else class="log-outbound-headers__empty">—</p>
  </div>
</template>

<style scoped>
.log-outbound-headers {
  min-width: 0;
}

.log-outbound-headers__label {
  margin: 0 0 4px;
  color: var(--color-text-faint);
  font-size: var(--text-label-xs);
}

.log-outbound-headers__list {
  display: grid;
  gap: 4px;
}

.log-outbound-headers__row {
  display: grid;
  min-width: 0;
  grid-template-columns: minmax(96px, max-content) minmax(0, 1fr);
  gap: 8px;
  align-items: start;
}

.log-outbound-headers__name {
  color: var(--color-text-muted);
  font-size: var(--text-label-xs);
  overflow-wrap: anywhere;
}

.log-outbound-headers__value {
  display: flex;
  min-width: 0;
  align-items: center;
  gap: 6px;
}

.log-outbound-headers__value-code {
  min-width: 0;
  flex: 1;
  overflow: hidden;
  color: var(--color-text);
  font-size: var(--text-label-xs);
  text-overflow: ellipsis;
  white-space: nowrap;
}

.log-outbound-headers__empty {
  color: var(--color-text-faint);
  font-size: var(--text-sm);
}

.log-outbound-headers__decode {
  flex: none;
  border: 1px solid var(--color-border-subtle);
  border-radius: var(--radius-control);
  background: var(--color-surface-raised);
  color: var(--color-text-muted);
  cursor: help;
  font: inherit;
  font-size: var(--text-label-xs);
  padding: 2px 6px;
}

.log-outbound-headers__decode:hover {
  border-color: var(--color-border-strong);
  color: var(--color-text);
}
</style>
