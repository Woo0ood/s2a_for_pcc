<template>
  <AppLayout>
    <div class="space-y-6">
      <div v-if="pageLoading" class="flex items-center justify-center py-16">
        <div class="h-8 w-8 animate-spin rounded-full border-b-2 border-primary-600"></div>
      </div>

      <template v-else>
        <!-- Header -->
        <div class="flex flex-col gap-4 lg:flex-row lg:items-center lg:justify-between">
          <div>
            <h1 class="text-2xl font-semibold text-gray-900 dark:text-white">{{ t('admin.payloadAudit.title') }}</h1>
            <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.description') }}</p>
          </div>
          <div class="flex flex-wrap items-center gap-2">
            <button type="button" class="btn btn-secondary inline-flex items-center gap-2" :disabled="statusLoading" @click="loadStatus">
              <Icon name="refresh" size="sm" :class="statusLoading ? 'animate-spin' : ''" />
              {{ t('admin.payloadAudit.refresh') }}
            </button>
            <button type="button" class="btn btn-primary inline-flex items-center gap-2" @click="configOpen = true">
              <Icon name="cog" size="sm" />
              {{ t('admin.payloadAudit.config') }}
            </button>
          </div>
        </div>

        <!-- Overview Cards -->
        <div class="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-4">
          <div
            v-for="item in overviewItems"
            :key="item.key"
            class="rounded-lg border border-gray-100 bg-white px-4 py-3 shadow-sm dark:border-dark-700 dark:bg-dark-800"
          >
            <div class="flex min-w-0 items-center gap-3">
              <div class="flex h-9 w-9 flex-shrink-0 items-center justify-center rounded-lg" :class="item.iconClass">
                <Icon :name="item.icon" size="sm" />
              </div>
              <div class="min-w-0 flex-1">
                <div class="flex min-w-0 items-center justify-between gap-2">
                  <p class="truncate text-xs font-medium text-gray-500 dark:text-gray-400">{{ item.label }}</p>
                  <span
                    v-if="item.badge"
                    class="inline-flex flex-shrink-0 items-center rounded-full px-2 py-0.5 text-xs font-medium"
                    :class="item.badgeClass"
                  >
                    {{ item.badge }}
                  </span>
                </div>
                <div class="mt-1 flex min-w-0 items-baseline gap-2">
                  <p class="truncate text-xl font-semibold leading-7 text-gray-900 dark:text-white">{{ item.value }}</p>
                  <p v-if="item.meta" class="truncate text-xs text-gray-500 dark:text-gray-400">{{ item.meta }}</p>
                </div>
              </div>
            </div>
          </div>
        </div>

        <!-- Filters (Collapsible) -->
        <div class="card">
          <div class="flex flex-col gap-4 border-b border-gray-100 px-6 py-4 dark:border-dark-700">
            <div class="flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
              <div>
                <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('admin.payloadAudit.records') }}</h2>
                <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.recordsHint') }}</p>
              </div>
              <div class="flex items-center gap-2">
                <button type="button" class="btn btn-secondary inline-flex items-center gap-2" @click="filtersExpanded = !filtersExpanded">
                  <Icon name="filter" size="sm" />
                  {{ t('admin.payloadAudit.filters') }}
                </button>
                <button type="button" class="btn btn-secondary inline-flex items-center gap-2" :disabled="listLoading" @click="loadList">
                  <Icon name="refresh" size="sm" :class="listLoading ? 'animate-spin' : ''" />
                  {{ t('admin.payloadAudit.refresh') }}
                </button>
              </div>
            </div>

            <div v-if="filtersExpanded" class="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-4">
              <input v-model="filters.from" type="datetime-local" class="input" :title="t('admin.payloadAudit.filterFrom')" />
              <input v-model="filters.to" type="datetime-local" class="input" :title="t('admin.payloadAudit.filterTo')" />
              <input v-model.number="filters.group_id" type="number" class="input" :placeholder="t('admin.payloadAudit.filterGroup')" />
              <input v-model.number="filters.user_id" type="number" class="input" :placeholder="t('admin.payloadAudit.filterUser')" />
              <input v-model.number="filters.api_key_id" type="number" class="input" :placeholder="t('admin.payloadAudit.filterApiKey')" />
              <input v-model.trim="filters.endpoint" type="text" class="input" :placeholder="t('admin.payloadAudit.filterEndpoint')" />
              <input v-model.trim="filters.model" type="text" class="input" :placeholder="t('admin.payloadAudit.filterModel')" />
              <input v-model.trim="filters.keyword" type="text" class="input" :placeholder="t('admin.payloadAudit.filterKeyword')" @keyup.enter="reloadFromFirst" />
              <div class="flex items-center gap-4">
                <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
                  <input v-model="filters.stream" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" :indeterminate="filters.stream === null" />
                  {{ t('admin.payloadAudit.table.stream') }}
                </label>
                <button type="button" class="btn btn-primary inline-flex items-center gap-2" @click="reloadFromFirst">
                  <Icon name="search" size="sm" />
                  {{ t('admin.payloadAudit.search') }}
                </button>
              </div>
            </div>
          </div>

          <!-- Table -->
          <div class="overflow-x-auto">
            <table class="min-w-full divide-y divide-gray-200 dark:divide-dark-700">
              <thead class="bg-gray-50 dark:bg-dark-800">
                <tr>
                  <th class="px-5 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.table.time') }}</th>
                  <th class="px-5 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.table.user') }}</th>
                  <th class="px-5 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.table.group') }}</th>
                  <th class="px-5 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.table.endpoint') }}</th>
                  <th class="px-5 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.table.model') }}</th>
                  <th class="px-5 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.table.status') }}</th>
                  <th class="px-5 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.table.stream') }}</th>
                  <th class="px-5 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.table.bytes') }}</th>
                  <th class="px-5 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.table.excerpt') }}</th>
                  <th class="px-5 py-3 text-left text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400"></th>
                </tr>
              </thead>
              <tbody class="divide-y divide-gray-100 bg-white dark:divide-dark-800 dark:bg-dark-800">
                <tr v-if="listLoading">
                  <td colspan="10" class="px-5 py-12 text-center text-sm text-gray-500 dark:text-gray-400">{{ t('common.loading') }}</td>
                </tr>
                <tr v-else-if="logs.length === 0">
                  <td colspan="10" class="px-5 py-12 text-center text-sm text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.emptyLogs') }}</td>
                </tr>
                <template v-else>
                  <tr v-for="row in logs" :key="row.ID" class="cursor-pointer hover:bg-gray-50 dark:hover:bg-dark-700/60" @click="openDetail(row)">
                    <td class="whitespace-nowrap px-5 py-4 text-sm text-gray-700 dark:text-gray-300">{{ formatDateTime(row.CreatedAt) }}</td>
                    <td class="whitespace-nowrap px-5 py-4 text-sm text-gray-700 dark:text-gray-300">
                      <div>{{ row.UserEmail || '-' }}</div>
                      <div v-if="row.APIKeyName" class="text-xs text-gray-400">{{ row.APIKeyName }}</div>
                    </td>
                    <td class="whitespace-nowrap px-5 py-4 text-sm text-gray-700 dark:text-gray-300">{{ row.GroupName || '-' }}</td>
                    <td class="whitespace-nowrap px-5 py-4 text-sm text-gray-700 dark:text-gray-300">{{ row.Endpoint || '-' }}</td>
                    <td class="whitespace-nowrap px-5 py-4 text-sm text-gray-700 dark:text-gray-300">
                      <div>{{ row.Model || '-' }}</div>
                      <div v-if="row.UpstreamModel && row.UpstreamModel !== row.Model" class="text-xs text-gray-400">{{ row.UpstreamModel }}</div>
                    </td>
                    <td class="whitespace-nowrap px-5 py-4">
                      <span
                        class="inline-flex rounded-md px-2 py-1 text-xs font-medium"
                        :class="statusBadgeClass(row.StatusCode)"
                      >
                        {{ row.StatusCode }}
                      </span>
                    </td>
                    <td class="whitespace-nowrap px-5 py-4 text-sm text-gray-700 dark:text-gray-300">
                      {{ row.Stream ? 'SSE' : '-' }}
                    </td>
                    <td class="whitespace-nowrap px-5 py-4 text-sm text-gray-700 dark:text-gray-300">
                      <div>{{ t('admin.payloadAudit.inputLabel') }} {{ formatBytes(row.InputBytes) }}</div>
                      <div class="text-xs text-gray-400">{{ t('admin.payloadAudit.outputLabel') }} {{ formatBytes(row.OutputBytes) }}</div>
                    </td>
                    <td class="max-w-xs px-5 py-4 text-sm text-gray-700 dark:text-gray-300">
                      <span class="block truncate" :title="row.InputExcerpt">{{ row.InputExcerpt || '-' }}</span>
                    </td>
                    <td class="whitespace-nowrap px-5 py-4">
                      <button type="button" class="text-primary-600 hover:text-primary-700 dark:text-primary-400" @click.stop="openDetail(row)">
                        <Icon name="eye" size="sm" />
                      </button>
                    </td>
                  </tr>
                </template>
              </tbody>
            </table>
          </div>

          <!-- Pagination -->
          <div v-if="logs.length > 0" class="flex items-center justify-between border-t border-gray-100 px-6 py-3 dark:border-dark-700">
            <span class="text-sm text-gray-500 dark:text-gray-400">
              {{ t('admin.payloadAudit.page', { page: currentPage }) }}
            </span>
            <div class="flex items-center gap-2">
              <button type="button" class="btn btn-secondary" :disabled="currentPage <= 1" @click="prevPage">
                {{ t('admin.payloadAudit.prevPage') }}
              </button>
              <button type="button" class="btn btn-secondary" :disabled="!nextCursor" @click="nextPage">
                {{ t('admin.payloadAudit.nextPage') }}
              </button>
            </div>
          </div>
        </div>
      </template>

      <!-- Detail Drawer (as BaseDialog wide) -->
      <BaseDialog :show="detailOpen" :title="t('admin.payloadAudit.detail.title')" width="extra-wide" @close="closeDetail">
        <div v-if="detailRow" class="space-y-4">
          <!-- Metadata -->
          <div class="rounded-lg border border-gray-100 p-4 dark:border-dark-700">
            <h3 class="mb-3 text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.payloadAudit.detail.metadata') }}</h3>
            <div class="grid grid-cols-1 gap-2 text-sm md:grid-cols-2 lg:grid-cols-3">
              <div>
                <span class="text-gray-500 dark:text-gray-400">Request ID: </span>
                <span class="font-mono text-gray-900 dark:text-white">{{ detailRow.RequestID }}</span>
                <button type="button" class="ml-1 text-primary-500 hover:text-primary-600" @click="copyText(detailRow.RequestID)">
                  <Icon name="copy" size="xs" />
                </button>
              </div>
              <div><span class="text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.table.user') }}: </span><span class="text-gray-900 dark:text-white">{{ detailRow.UserEmail || '-' }}</span></div>
              <div><span class="text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.table.group') }}: </span><span class="text-gray-900 dark:text-white">{{ detailRow.GroupName || '-' }}</span></div>
              <div><span class="text-gray-500 dark:text-gray-400">API Key: </span><span class="text-gray-900 dark:text-white">{{ detailRow.APIKeyName || '-' }}</span></div>
              <div><span class="text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.table.endpoint') }}: </span><span class="text-gray-900 dark:text-white">{{ detailRow.Endpoint }}</span></div>
              <div><span class="text-gray-500 dark:text-gray-400">Provider: </span><span class="text-gray-900 dark:text-white">{{ detailRow.Provider }}</span></div>
              <div><span class="text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.table.model') }}: </span><span class="text-gray-900 dark:text-white">{{ detailRow.Model }}</span></div>
              <div><span class="text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.table.status') }}: </span><span class="text-gray-900 dark:text-white">{{ detailRow.StatusCode }}</span></div>
              <div><span class="text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.duration') }}: </span><span class="text-gray-900 dark:text-white">{{ detailRow.DurationMs }} ms</span></div>
              <div><span class="text-gray-500 dark:text-gray-400">Client IP: </span><span class="font-mono text-gray-900 dark:text-white">{{ detailRow.ClientIP }}</span></div>
              <div v-if="detailRow.ErrorMessage"><span class="text-gray-500 dark:text-gray-400">Error: </span><span class="text-red-600 dark:text-red-400">{{ detailRow.ErrorMessage }}</span></div>
            </div>
          </div>

          <!-- Tabs: Input / Output / Raw JSON -->
          <div class="flex gap-2 border-b border-gray-100 pb-3 dark:border-dark-700">
            <button
              v-for="tab in detailTabs"
              :key="tab"
              type="button"
              class="inline-flex whitespace-nowrap rounded-lg px-3 py-2 text-sm font-medium transition-colors"
              :class="activeDetailTab === tab ? 'bg-primary-50 text-primary-700 dark:bg-primary-900/30 dark:text-primary-300' : 'text-gray-500 hover:bg-gray-50 hover:text-gray-900 dark:text-gray-400 dark:hover:bg-dark-700 dark:hover:text-white'"
              @click="activeDetailTab = tab"
            >
              {{ detailTabLabel(tab) }}
            </button>
          </div>

          <!-- Tab Content -->
          <div v-if="activeDetailTab === 'input'">
            <div class="flex items-center justify-between gap-2 pb-2">
              <span class="text-sm text-gray-500 dark:text-gray-400">
                {{ formatBytes(detailRow.InputBytes) }}
                <span v-if="detailRow.InputTruncated" class="text-amber-600 dark:text-amber-400"> ({{ t('admin.payloadAudit.detail.truncated') }})</span>
              </span>
              <button
                v-if="!detailFullLoaded"
                type="button"
                class="btn btn-secondary inline-flex items-center gap-2"
                :disabled="detailFullLoading"
                @click="loadFullPayload"
              >
                <Icon name="download" size="sm" :class="detailFullLoading ? 'animate-spin' : ''" />
                {{ t('admin.payloadAudit.detail.expandFull') }}
              </button>
            </div>
            <pre class="max-h-[420px] overflow-auto whitespace-pre-wrap break-words rounded-lg bg-gray-950 p-4 text-sm leading-6 text-gray-100 shadow-inner dark:bg-black/50">{{ detailFullLoaded ? detailFull!.InputBody : detailRow.InputExcerpt }}</pre>
          </div>

          <div v-else-if="activeDetailTab === 'output'">
            <div class="flex items-center justify-between gap-2 pb-2">
              <span class="text-sm text-gray-500 dark:text-gray-400">
                {{ formatBytes(detailRow.OutputBytes) }}
                <span v-if="detailRow.OutputTruncated" class="text-amber-600 dark:text-amber-400"> ({{ t('admin.payloadAudit.detail.truncated') }})</span>
                <span v-if="detailRow.OutputOmitted" class="text-amber-600 dark:text-amber-400"> ({{ t('admin.payloadAudit.detail.omitted') }})</span>
              </span>
              <button
                v-if="!detailFullLoaded"
                type="button"
                class="btn btn-secondary inline-flex items-center gap-2"
                :disabled="detailFullLoading"
                @click="loadFullPayload"
              >
                <Icon name="download" size="sm" :class="detailFullLoading ? 'animate-spin' : ''" />
                {{ t('admin.payloadAudit.detail.expandFull') }}
              </button>
            </div>
            <pre class="max-h-[420px] overflow-auto whitespace-pre-wrap break-words rounded-lg bg-gray-950 p-4 text-sm leading-6 text-gray-100 shadow-inner dark:bg-black/50">{{ detailFullLoaded ? detailFull!.OutputBody : detailRow.OutputExcerpt }}</pre>
          </div>

          <div v-else>
            <pre class="max-h-[420px] overflow-auto whitespace-pre-wrap break-words rounded-lg bg-gray-950 p-4 text-sm leading-6 text-gray-100 shadow-inner dark:bg-black/50">{{ JSON.stringify(detailFullLoaded ? detailFull : detailRow, null, 2) }}</pre>
          </div>
        </div>

        <template #footer>
          <div class="flex justify-between">
            <button type="button" class="btn btn-secondary inline-flex items-center gap-2" :disabled="exporting" @click="exportConversation">
              <Icon name="download" size="sm" :class="exporting ? 'animate-spin' : ''" />
              {{ exporting ? t('admin.payloadAudit.exporting') : t('admin.payloadAudit.exportConversation') }}
            </button>
            <button type="button" class="btn btn-secondary" @click="closeDetail">{{ t('common.close') }}</button>
          </div>
        </template>
      </BaseDialog>

      <!-- Config Dialog -->
      <BaseDialog :show="configOpen" :title="t('admin.payloadAudit.configTitle')" width="extra-wide" @close="configOpen = false">
        <div class="space-y-6">
          <!-- Config Tabs -->
          <div class="flex gap-2 overflow-x-auto border-b border-gray-100 pb-3 dark:border-dark-700">
            <button
              v-for="tab in configTabs"
              :key="tab.id"
              type="button"
              class="inline-flex whitespace-nowrap rounded-lg px-3 py-2 text-sm font-medium transition-colors"
              :class="activeConfigTab === tab.id ? 'bg-primary-50 text-primary-700 dark:bg-primary-900/30 dark:text-primary-300' : 'text-gray-500 hover:bg-gray-50 hover:text-gray-900 dark:text-gray-400 dark:hover:bg-dark-700 dark:hover:text-white'"
              @click="activeConfigTab = tab.id"
            >
              {{ tab.label }}
            </button>
          </div>

          <!-- Basic Tab -->
          <div v-if="activeConfigTab === 'basic'" class="space-y-5">
            <div class="grid grid-cols-1 gap-5 lg:grid-cols-2">
              <div class="flex items-center justify-between rounded-lg border border-gray-100 p-4 dark:border-dark-700">
                <div>
                  <p class="text-sm font-medium text-gray-900 dark:text-white">{{ t('admin.payloadAudit.configEnabled') }}</p>
                  <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.configEnabledHint') }}</p>
                </div>
                <Toggle v-model="configForm.enabled" />
              </div>
              <div class="flex items-center justify-between rounded-lg border border-gray-100 p-4 dark:border-dark-700">
                <div>
                  <p class="text-sm font-medium text-gray-900 dark:text-white">{{ t('admin.payloadAudit.configAllGroups') }}</p>
                  <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.configAllGroupsHint') }}</p>
                </div>
                <Toggle v-model="configForm.config.all_groups" />
              </div>
              <div>
                <label class="input-label">{{ t('admin.payloadAudit.configExcerptBytes') }}</label>
                <input v-model.number="configForm.config.excerpt_bytes" type="number" min="0" class="input" />
              </div>
              <div>
                <label class="input-label">{{ t('admin.payloadAudit.configRetentionDays') }}</label>
                <input v-model.number="configForm.config.retention_days" type="number" min="1" class="input" />
              </div>
              <div>
                <label class="input-label">{{ t('admin.payloadAudit.configInputMaxBytes') }}</label>
                <input v-model.number="configForm.config.input_max_bytes" type="number" min="0" class="input" />
              </div>
              <div>
                <label class="input-label">{{ t('admin.payloadAudit.configOutputMaxBytes') }}</label>
                <input v-model.number="configForm.config.output_max_bytes" type="number" min="0" class="input" />
              </div>
            </div>
          </div>

          <!-- Performance Tab -->
          <div v-if="activeConfigTab === 'performance'" class="space-y-5">
            <div class="grid grid-cols-1 gap-5 lg:grid-cols-2">
              <div>
                <label class="input-label">{{ t('admin.payloadAudit.configWorkerCount') }}</label>
                <input v-model.number="configForm.config.worker_count" type="number" min="1" class="input" />
              </div>
              <div>
                <label class="input-label">{{ t('admin.payloadAudit.configQueueSize') }}</label>
                <input v-model.number="configForm.config.queue_size" type="number" min="1" class="input" />
              </div>
              <div>
                <label class="input-label">{{ t('admin.payloadAudit.configQueueMaxBytes') }}</label>
                <input v-model.number="configForm.config.queue_max_bytes" type="number" min="0" class="input" />
              </div>
              <div>
                <label class="input-label">{{ t('admin.payloadAudit.configBatchSize') }}</label>
                <input v-model.number="configForm.config.batch_size" type="number" min="1" class="input" />
              </div>
              <div>
                <label class="input-label">{{ t('admin.payloadAudit.configBatchFlushMs') }}</label>
                <input v-model.number="configForm.config.batch_flush_ms" type="number" min="100" class="input" />
              </div>
            </div>

            <!-- External export worker -->
            <div class="rounded-lg border border-gray-100 p-4 dark:border-dark-700">
              <h4 class="mb-1 text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.payloadAudit.exportWorker') }}</h4>
              <p class="mb-4 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.exportWorkerHint') }}</p>
              <div class="grid grid-cols-1 gap-4 lg:grid-cols-2">
                <div>
                  <label class="input-label">{{ t('admin.payloadAudit.exportWorkerUrl') }}</label>
                  <input
                    v-model.trim="configForm.config.export_worker_url"
                    type="text"
                    class="input"
                    placeholder="https://export-worker.example.com"
                  />
                </div>
                <div>
                  <label class="input-label">{{ t('admin.payloadAudit.exportWorkerToken') }}</label>
                  <input
                    v-model.trim="configForm.config.export_worker_token"
                    type="text"
                    class="input"
                    autocomplete="off"
                  />
                </div>
              </div>
            </div>
          </div>

          <!-- Offload Tab -->
          <div v-if="activeConfigTab === 'offload'" class="space-y-5">
            <!-- Independent-from-backup hint -->
            <p class="rounded-lg border border-blue-100 bg-blue-50 px-4 py-3 text-sm text-blue-700 dark:border-blue-900/40 dark:bg-blue-900/20 dark:text-blue-300">
              {{ t('admin.payloadAudit.offloadS3IndependentHint') }}
            </p>

            <!-- Enable switch -->
            <div class="flex items-center justify-between rounded-lg border border-gray-100 p-4 dark:border-dark-700">
              <div>
                <p class="text-sm font-medium text-gray-900 dark:text-white">{{ t('admin.payloadAudit.offloadEnabled') }}</p>
              </div>
              <Toggle v-model="configForm.config.offload_enabled" @update:model-value="(v: boolean) => { if (v) ensureBlobStore() }" />
            </div>

            <!-- Offload sub-fields (only shown when enabled) -->
            <template v-if="configForm.config.offload_enabled">
              <div class="grid grid-cols-1 gap-5 lg:grid-cols-2">
                <div>
                  <label class="input-label">{{ t('admin.payloadAudit.blobOffloadMinBytes') }}</label>
                  <input v-model.number="configForm.config.blob_offload_min_bytes" type="number" min="0" class="input" />
                </div>
                <div>
                  <label class="input-label">{{ t('admin.payloadAudit.blobStorePrefix') }}</label>
                  <input v-model.trim="configForm.config.blob_store_prefix" type="text" class="input" />
                </div>
                <div>
                  <label class="input-label">{{ t('admin.payloadAudit.offloadRetentionMarginDays') }}</label>
                  <input v-model.number="configForm.config.offload_retention_margin_days" type="number" min="0" class="input" />
                </div>
              </div>

              <!-- S3 block -->
              <div class="rounded-lg border border-gray-100 p-4 dark:border-dark-700">
                <h4 class="mb-4 text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.payloadAudit.blobStoreS3') }}</h4>
                <div class="grid grid-cols-1 gap-4 lg:grid-cols-2">
                  <div>
                    <label class="input-label">{{ t('admin.payloadAudit.s3Endpoint') }}</label>
                    <input v-model.trim="configForm.config.blob_store!.endpoint" type="text" class="input" />
                  </div>
                  <div>
                    <label class="input-label">{{ t('admin.payloadAudit.s3Region') }}</label>
                    <input v-model.trim="configForm.config.blob_store!.region" type="text" class="input" />
                  </div>
                  <div>
                    <label class="input-label">{{ t('admin.payloadAudit.s3Bucket') }}</label>
                    <input v-model.trim="configForm.config.blob_store!.bucket" type="text" class="input" />
                  </div>
                  <div>
                    <label class="input-label">{{ t('admin.payloadAudit.s3AccessKeyId') }}</label>
                    <input v-model.trim="configForm.config.blob_store!.access_key_id" type="text" class="input" />
                  </div>
                  <div>
                    <label class="input-label">{{ t('admin.payloadAudit.s3SecretAccessKey') }}</label>
                    <input
                      v-model="configForm.config.blob_store!.secret_access_key"
                      type="password"
                      class="input"
                      :placeholder="t('admin.payloadAudit.s3SecretPlaceholder')"
                      autocomplete="new-password"
                    />
                  </div>
                  <div>
                    <label class="input-label">{{ t('admin.payloadAudit.s3Prefix') }}</label>
                    <input v-model.trim="configForm.config.blob_store!.prefix" type="text" class="input" />
                  </div>
                  <div class="flex items-center justify-between rounded-lg border border-gray-100 p-3 dark:border-dark-700 lg:col-span-2">
                    <p class="text-sm text-gray-700 dark:text-gray-300">{{ t('admin.payloadAudit.s3ForcePathStyle') }}</p>
                    <Toggle v-model="configForm.config.blob_store!.force_path_style" />
                  </div>
                </div>
              </div>
            </template>
          </div>

          <!-- API Keys Tab -->
          <div v-if="activeConfigTab === 'apiKey'" class="space-y-5">
            <div class="flex items-center justify-between">
              <h3 class="text-sm font-semibold text-gray-900 dark:text-white">{{ t('admin.payloadAudit.exportKeys') }}</h3>
              <button type="button" class="btn btn-primary inline-flex items-center gap-2" @click="createKeyOpen = true">
                <Icon name="plus" size="sm" />
                {{ t('admin.payloadAudit.createKey') }}
              </button>
            </div>

            <div v-if="exportKeysLoading" class="py-6 text-center text-sm text-gray-500 dark:text-gray-400">{{ t('common.loading') }}</div>
            <div v-else-if="exportKeys.length === 0" class="py-6 text-center text-sm text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.noExportKeys') }}</div>
            <div v-else class="overflow-x-auto">
              <table class="min-w-full divide-y divide-gray-200 dark:divide-dark-700">
                <thead class="bg-gray-50 dark:bg-dark-800">
                  <tr>
                    <th class="px-4 py-2 text-left text-xs font-medium text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.keyName') }}</th>
                    <th class="px-4 py-2 text-left text-xs font-medium text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.keyRateLimit') }}</th>
                    <th class="px-4 py-2 text-left text-xs font-medium text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.keyCreatedAt') }}</th>
                    <th class="px-4 py-2 text-left text-xs font-medium text-gray-500 dark:text-gray-400">{{ t('admin.payloadAudit.keyLastUsed') }}</th>
                    <th class="px-4 py-2 text-left text-xs font-medium text-gray-500 dark:text-gray-400"></th>
                  </tr>
                </thead>
                <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
                  <tr v-for="key in exportKeys" :key="key.id">
                    <td class="px-4 py-2 text-sm text-gray-900 dark:text-white">{{ key.name }}</td>
                    <td class="px-4 py-2 text-sm text-gray-700 dark:text-gray-300">{{ key.rate_limit_per_min }}/min</td>
                    <td class="px-4 py-2 text-sm text-gray-700 dark:text-gray-300">{{ formatDateTime(key.created_at) }}</td>
                    <td class="px-4 py-2 text-sm text-gray-700 dark:text-gray-300">{{ key.last_used_at ? formatDateTime(key.last_used_at) : '-' }}</td>
                    <td class="px-4 py-2">
                      <button type="button" class="text-red-600 hover:text-red-700 dark:text-red-400" @click="confirmDeleteKey(key)">
                        <Icon name="trash" size="sm" />
                      </button>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>

            <div class="flex items-center gap-2 border-t border-gray-100 pt-4 dark:border-dark-700">
              <button type="button" class="btn btn-secondary inline-flex items-center gap-2" :disabled="cleanupLoading" @click="runCleanup">
                <Icon name="trash" size="sm" :class="cleanupLoading ? 'animate-spin' : ''" />
                {{ t('admin.payloadAudit.runCleanup') }}
              </button>
            </div>
          </div>
        </div>

        <template #footer>
          <div class="flex justify-end gap-2">
            <button type="button" class="btn btn-secondary" @click="configOpen = false">{{ t('common.cancel') }}</button>
            <button v-if="activeConfigTab !== 'apiKey'" type="button" class="btn btn-primary" :disabled="configSaving" @click="saveConfig">
              {{ t('common.save') }}
            </button>
          </div>
        </template>
      </BaseDialog>

      <!-- Create Export Key Dialog -->
      <BaseDialog :show="createKeyOpen" :title="t('admin.payloadAudit.createKey')" width="narrow" @close="createKeyOpen = false">
        <div class="space-y-4">
          <div>
            <label class="input-label">{{ t('admin.payloadAudit.keyName') }}</label>
            <input v-model.trim="newKeyName" type="text" class="input" :placeholder="t('admin.payloadAudit.keyNamePlaceholder')" />
          </div>
          <div>
            <label class="input-label">{{ t('admin.payloadAudit.keyRateLimit') }}</label>
            <input v-model.number="newKeyRate" type="number" min="1" class="input" placeholder="60" />
          </div>
        </div>
        <template #footer>
          <div class="flex justify-end gap-2">
            <button type="button" class="btn btn-secondary" @click="createKeyOpen = false">{{ t('common.cancel') }}</button>
            <button type="button" class="btn btn-primary" :disabled="creatingKey || !newKeyName" @click="doCreateKey">
              {{ t('admin.payloadAudit.createKey') }}
            </button>
          </div>
        </template>
      </BaseDialog>

      <!-- Show Token Dialog (one-time display) -->
      <BaseDialog :show="tokenDialogOpen" :title="t('admin.payloadAudit.tokenCreated')" width="normal" @close="tokenDialogOpen = false">
        <div class="space-y-4">
          <p class="text-sm text-amber-700 dark:text-amber-300">{{ t('admin.payloadAudit.tokenWarning') }}</p>
          <div class="flex items-center gap-2 rounded-lg bg-gray-50 p-3 dark:bg-dark-700">
            <code class="flex-1 break-all font-mono text-sm text-gray-900 dark:text-white">{{ createdToken }}</code>
            <button type="button" class="btn btn-secondary" @click="copyText(createdToken)">
              <Icon name="copy" size="sm" />
            </button>
          </div>
        </div>
        <template #footer>
          <div class="flex justify-end">
            <button type="button" class="btn btn-primary" @click="tokenDialogOpen = false">{{ t('admin.payloadAudit.tokenCopied') }}</button>
          </div>
        </template>
      </BaseDialog>

      <!-- Delete Key Confirm Dialog -->
      <BaseDialog :show="deleteKeyOpen" :title="t('admin.payloadAudit.confirmDelete')" width="narrow" @close="deleteKeyOpen = false">
        <p class="text-sm text-gray-700 dark:text-gray-300">{{ t('admin.payloadAudit.confirmDeleteMsg', { name: deleteKeyTarget?.name ?? '' }) }}</p>
        <template #footer>
          <div class="flex justify-end gap-2">
            <button type="button" class="btn btn-secondary" @click="deleteKeyOpen = false">{{ t('common.cancel') }}</button>
            <button type="button" class="btn bg-red-600 text-white hover:bg-red-700" :disabled="deletingKey" @click="doDeleteKey">
              {{ t('admin.payloadAudit.deleteKey') }}
            </button>
          </div>
        </template>
      </BaseDialog>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import Toggle from '@/components/common/Toggle.vue'
import {
  payloadAuditAPI,
  type PayloadAuditLog,
  type PayloadAuditConfigEnvelope,
  type PayloadAuditStatus,
  type PayloadAuditExportKey,
  type BlobStoreConfig,
} from '@/api/admin/payloadAudit'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatDateTime as formatDateTimeValue } from '@/utils/format'

type ConfigTab = 'basic' | 'performance' | 'offload' | 'apiKey'
type DetailTab = 'input' | 'output' | 'raw'

const { t } = useI18n()
const appStore = useAppStore()

// --- Page State ---
const pageLoading = ref(true)
const statusLoading = ref(false)
const listLoading = ref(false)
const configSaving = ref(false)
const configOpen = ref(false)
const activeConfigTab = ref<ConfigTab>('basic')
const filtersExpanded = ref(false)

// --- Status ---
const status = ref<PayloadAuditStatus | null>(null)
const configEnvelope = ref<PayloadAuditConfigEnvelope | null>(null)

// --- List ---
const logs = ref<PayloadAuditLog[]>([])
const currentPage = ref(1)
const nextCursor = ref('')
const cursorStack = ref<string[]>([]) // stack for going back
const pageSize = 20

const filters = reactive({
  from: defaultFrom(),
  to: '',
  user_id: null as number | null,
  group_id: null as number | null,
  api_key_id: null as number | null,
  endpoint: '',
  model: '',
  keyword: '',
  stream: null as boolean | null,
})

// --- Detail ---
const detailOpen = ref(false)
const detailRow = ref<PayloadAuditLog | null>(null)
const detailFull = ref<PayloadAuditLog | null>(null)
const detailFullLoaded = ref(false)
const detailFullLoading = ref(false)
const activeDetailTab = ref<DetailTab>('input')
const detailTabs: DetailTab[] = ['input', 'output', 'raw']

// --- Config Form ---
const configForm = reactive<PayloadAuditConfigEnvelope>({
  enabled: false,
  config: {
    all_groups: true,
    group_ids: [],
    input_max_bytes: 0,
    output_max_bytes: 0,
    excerpt_bytes: 512,
    retention_days: 7,
    worker_count: 2,
    queue_size: 1024,
    queue_max_bytes: 0,
    batch_size: 50,
    batch_flush_ms: 1000,
    export_api_keys: [],
    offload_enabled: false,
    blob_offload_min_bytes: 0,
    blob_store_prefix: 'payload-audit/',
    offload_retention_margin_days: 0,
    blob_store: undefined,
    export_worker_url: '',
    export_worker_token: '',
  },
})

function defaultBlobStore(): BlobStoreConfig {
  return { endpoint: '', region: '', bucket: '', access_key_id: '', secret_access_key: '', prefix: '', force_path_style: false }
}

function ensureBlobStore() {
  if (!configForm.config.blob_store) {
    configForm.config.blob_store = defaultBlobStore()
  }
}

// --- Export Keys ---
const exportKeys = ref<PayloadAuditExportKey[]>([])
const exportKeysLoading = ref(false)
const createKeyOpen = ref(false)
const newKeyName = ref('')
const newKeyRate = ref(60)
const creatingKey = ref(false)
const createdToken = ref('')
const tokenDialogOpen = ref(false)
const deleteKeyOpen = ref(false)
const deleteKeyTarget = ref<PayloadAuditExportKey | null>(null)
const deletingKey = ref(false)
const cleanupLoading = ref(false)
const exporting = ref(false)

// --- Config Tabs ---
const configTabs = computed(() => [
  { id: 'basic' as const, label: t('admin.payloadAudit.tabs.basic') },
  { id: 'performance' as const, label: t('admin.payloadAudit.tabs.performance') },
  { id: 'offload' as const, label: t('admin.payloadAudit.tabs.offload') },
  { id: 'apiKey' as const, label: t('admin.payloadAudit.tabs.apiKey') },
])

// --- Overview ---
type OverviewItem = {
  key: string
  label: string
  value: string
  meta: string
  icon: 'shield' | 'chart' | 'database' | 'exclamationTriangle'
  iconClass: string
  badge?: string
  badgeClass?: string
}

const overviewItems = computed((): OverviewItem[] => {
  const s = status.value
  const enabled = configEnvelope.value?.enabled ?? false
  return [
    {
      key: 'status',
      label: t('admin.payloadAudit.overview.status'),
      value: enabled ? t('admin.payloadAudit.overview.enabled') : t('admin.payloadAudit.overview.disabled'),
      meta: '',
      icon: 'shield',
      iconClass: enabled ? 'bg-emerald-50 text-emerald-600 dark:bg-emerald-900/30 dark:text-emerald-300' : 'bg-gray-100 text-gray-500 dark:bg-dark-700 dark:text-gray-400',
      badge: enabled ? t('admin.payloadAudit.overview.on') : t('admin.payloadAudit.overview.off'),
      badgeClass: enabled ? 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300' : 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300',
    },
    {
      key: 'recorded',
      label: t('admin.payloadAudit.overview.recorded24h'),
      value: formatNumber(s?.stats_24h?.BatchInserted ?? 0),
      meta: t('admin.payloadAudit.overview.accepted', { n: s?.stats_24h?.Accepted ?? 0 }),
      icon: 'chart',
      iconClass: 'bg-blue-50 text-blue-600 dark:bg-blue-900/30 dark:text-blue-300',
    },
    {
      key: 'queue',
      label: t('admin.payloadAudit.overview.queueUsage'),
      value: (s?.queue?.usage_pct ?? 0).toFixed(1) + '%',
      meta: `${formatNumber(s?.queue?.depth ?? 0)} / ${formatNumber(s?.queue?.size ?? 0)}`,
      icon: 'database',
      iconClass: 'bg-purple-50 text-purple-600 dark:bg-purple-900/30 dark:text-purple-300',
    },
    {
      key: 'dropped',
      label: t('admin.payloadAudit.overview.dropped24h'),
      value: formatNumber((s?.stats_24h?.DropQueueFull ?? 0) + (s?.stats_24h?.DropByteBudget ?? 0)),
      meta: '',
      icon: 'exclamationTriangle',
      iconClass: (s?.stats_24h?.DropQueueFull ?? 0) + (s?.stats_24h?.DropByteBudget ?? 0) > 0
        ? 'bg-amber-50 text-amber-600 dark:bg-amber-900/30 dark:text-amber-300'
        : 'bg-gray-100 text-gray-500 dark:bg-dark-700 dark:text-gray-400',
    },
  ]
})

// --- Helpers ---

function defaultFrom(): string {
  const d = new Date()
  d.setDate(d.getDate() - 1)
  return toLocalDatetimeInput(d)
}

function toLocalDatetimeInput(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
}

function toRFC3339(localInput: string): string {
  if (!localInput) return ''
  return new Date(localInput).toISOString()
}

function formatDateTime(v: string | null | undefined): string {
  return formatDateTimeValue(v) || '-'
}

function formatNumber(n: number): string {
  return n.toLocaleString()
}

function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB']
  const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1)
  return (bytes / Math.pow(1024, i)).toFixed(i === 0 ? 0 : 1) + ' ' + units[i]
}

function statusBadgeClass(code: number): string {
  if (code >= 200 && code < 300) return 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/20 dark:text-emerald-300'
  if (code >= 400 && code < 500) return 'bg-amber-50 text-amber-700 dark:bg-amber-900/20 dark:text-amber-300'
  return 'bg-red-50 text-red-700 dark:bg-red-900/20 dark:text-red-300'
}

function detailTabLabel(tab: DetailTab): string {
  const map: Record<DetailTab, string> = {
    input: t('admin.payloadAudit.detail.inputTab'),
    output: t('admin.payloadAudit.detail.outputTab'),
    raw: t('admin.payloadAudit.detail.rawJsonTab'),
  }
  return map[tab]
}

async function copyText(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    appStore.showSuccess(t('admin.payloadAudit.copied'))
  } catch {
    // fallback: ignore
  }
}

// --- Data Loading ---

async function loadStatus() {
  statusLoading.value = true
  try {
    status.value = await payloadAuditAPI.getStatus()
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('admin.payloadAudit.statusFailed')))
  } finally {
    statusLoading.value = false
  }
}

async function loadConfig() {
  try {
    const envelope = await payloadAuditAPI.getConfig()
    configEnvelope.value = envelope
    configForm.enabled = envelope.enabled
    // Assign scalar fields and explicit offload fields.
    Object.assign(configForm.config, envelope.config)
    // GET masks secret_access_key to "". Keep it as "" so the user must
    // type a new value to change it; blank means "keep existing" on save.
    if (envelope.config.blob_store) {
      configForm.config.blob_store = { ...envelope.config.blob_store, secret_access_key: '' }
    } else {
      configForm.config.blob_store = undefined
    }
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('admin.payloadAudit.loadFailed')))
  }
}

async function loadList() {
  listLoading.value = true
  try {
    const params = {
      from: toRFC3339(filters.from) || new Date(Date.now() - 86400000).toISOString(),
      to: filters.to ? toRFC3339(filters.to) : new Date().toISOString(),
      include_body: 'excerpt' as const,
      limit: pageSize,
      cursor: currentPage.value > 1 ? nextCursor.value : undefined,
      user_id: filters.user_id || undefined,
      group_id: filters.group_id || undefined,
      api_key_id: filters.api_key_id || undefined,
      keyword: filters.keyword || undefined,
    }
    const res = await payloadAuditAPI.listPayloads(params)
    logs.value = res.data ?? []
    nextCursor.value = res.next_cursor ?? ''
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('admin.payloadAudit.listFailed')))
  } finally {
    listLoading.value = false
  }
}

function reloadFromFirst() {
  currentPage.value = 1
  cursorStack.value = []
  nextCursor.value = ''
  loadList()
}

function nextPage() {
  if (!nextCursor.value) return
  cursorStack.value.push(nextCursor.value)
  currentPage.value++
  loadList()
}

function prevPage() {
  if (currentPage.value <= 1) return
  cursorStack.value.pop()
  currentPage.value--
  nextCursor.value = cursorStack.value[cursorStack.value.length - 1] ?? ''
  loadList()
}

// --- Detail ---

function openDetail(row: PayloadAuditLog) {
  detailRow.value = row
  detailFull.value = null
  detailFullLoaded.value = false
  activeDetailTab.value = 'input'
  detailOpen.value = true
}

function closeDetail() {
  detailOpen.value = false
  detailRow.value = null
  detailFull.value = null
  detailFullLoaded.value = false
}

async function loadFullPayload() {
  if (!detailRow.value) return
  detailFullLoading.value = true
  try {
    detailFull.value = await payloadAuditAPI.getPayload(detailRow.value.ID, detailRow.value.CreatedAt)
    detailFullLoaded.value = true
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('admin.payloadAudit.loadPayloadFailed')))
  } finally {
    detailFullLoading.value = false
  }
}

async function exportConversation() {
  if (!detailRow.value || exporting.value) return
  exporting.value = true
  // Open the new tab synchronously within the user-gesture to avoid popup blockers.
  const win = window.open('', '_blank')
  if (win) win.document.write('<p style="font-family:sans-serif;padding:2rem">Exporting…</p>')
  try {
    const tz = Intl.DateTimeFormat().resolvedOptions().timeZone
    const { job_id } = await payloadAuditAPI.startConversationExport(
      detailRow.value.ID,
      detailRow.value.CreatedAt,
      tz
    )
    const deadline = Date.now() + 5 * 60 * 1000
    let html: string | null = null
    while (Date.now() < deadline) {
      await new Promise<void>(r => setTimeout(r, 1500))
      const st = await payloadAuditAPI.getConversationExportStatus(job_id)
      if (st.status === 'done') {
        html = await payloadAuditAPI.getConversationExportResult(job_id)
        break
      }
      if (st.status === 'failed') throw new Error(st.error || 'export failed')
    }
    if (html === null) throw new Error('export timed out')
    const url = URL.createObjectURL(new Blob([html], { type: 'text/html' }))
    if (win) {
      win.location.href = url
    } else {
      window.open(url, '_blank')
    }
    setTimeout(() => URL.revokeObjectURL(url), 60_000)
  } catch (err) {
    if (win) win.close()
    appStore.showError(extractApiErrorMessage(err, t('admin.payloadAudit.exportConversationFailed')))
  } finally {
    exporting.value = false
  }
}

// --- Config ---

async function saveConfig() {
  configSaving.value = true
  try {
    const result = await payloadAuditAPI.updateConfig({
      enabled: configForm.enabled,
      config: { ...configForm.config },
    })
    appStore.showSuccess(t('admin.payloadAudit.saved'))
    if (result.need_rebuild_sink) {
      appStore.showSuccess(t('admin.payloadAudit.sinkRebuild'))
    }
    configEnvelope.value = { enabled: configForm.enabled, config: { ...configForm.config } }
    await loadStatus()
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('admin.payloadAudit.saveFailed')))
  } finally {
    configSaving.value = false
  }
}

// --- Export Keys ---

async function loadExportKeys() {
  exportKeysLoading.value = true
  try {
    exportKeys.value = await payloadAuditAPI.listExportKeys()
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('admin.payloadAudit.loadKeysFailed')))
  } finally {
    exportKeysLoading.value = false
  }
}

async function doCreateKey() {
  if (!newKeyName.value) return
  creatingKey.value = true
  try {
    const res = await payloadAuditAPI.createExportKey(newKeyName.value, newKeyRate.value || 60)
    createdToken.value = res.token
    createKeyOpen.value = false
    tokenDialogOpen.value = true
    newKeyName.value = ''
    newKeyRate.value = 60
    await loadExportKeys()
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('admin.payloadAudit.createKeyFailed')))
  } finally {
    creatingKey.value = false
  }
}

function confirmDeleteKey(key: PayloadAuditExportKey) {
  deleteKeyTarget.value = key
  deleteKeyOpen.value = true
}

async function doDeleteKey() {
  if (!deleteKeyTarget.value) return
  deletingKey.value = true
  try {
    await payloadAuditAPI.deleteExportKey(deleteKeyTarget.value.id)
    deleteKeyOpen.value = false
    deleteKeyTarget.value = null
    await loadExportKeys()
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('admin.payloadAudit.deleteKeyFailed')))
  } finally {
    deletingKey.value = false
  }
}

async function runCleanup() {
  cleanupLoading.value = true
  try {
    const res = await payloadAuditAPI.runCleanup()
    appStore.showSuccess(t('admin.payloadAudit.cleanupDone', { deleted: res.deleted, ms: res.duration_ms }))
  } catch (err) {
    appStore.showError(extractApiErrorMessage(err, t('admin.payloadAudit.cleanupFailed')))
  } finally {
    cleanupLoading.value = false
  }
}

// --- Init ---

onMounted(async () => {
  try {
    await Promise.all([loadConfig(), loadStatus()])
    await loadList()
    await loadExportKeys()
  } finally {
    pageLoading.value = false
  }
})
</script>
