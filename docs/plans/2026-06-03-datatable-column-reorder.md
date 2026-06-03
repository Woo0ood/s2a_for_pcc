# DataTable 可拖动列序 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让所有走 `DataTable.vue` 的表格（18 个）支持「从专用手柄拖动表头来重排列顺序」，顺序持久化到 localStorage，纯前端。

**Architecture:** 排序/钉位/合并的纯逻辑放在新 util `utils/columnOrder.ts`（可单测）；`DataTable.vue` 渲染改为基于本地 `renderColumns` ref（由 util 从 `props.columns` + 保存的顺序算出），用 `vue-draggable-plus` 的 `useDraggable` 绑定表头 `<tr>`，仅 `.col-drag-handle` 可拖、`select`/`actions` 列钉死两端。调用页零改动 → 日后 commit 可干净 cherry-pick 到 upstream。

**Tech Stack:** Vue 3 `<script setup lang="ts">`, vitest + @vue/test-utils, `vue-draggable-plus@0.6.1`（仅有 `VueDraggable` / `useDraggable` / `vDraggable`，**无** `useSortable`），`vue-router`（`useRoute`）。

**所有命令在 `frontend/` 目录下执行。**

---

## File Structure

- **Create** `frontend/src/utils/columnOrder.ts` — 纯逻辑：`applyColumnOrder`、storage key 构造、读写 localStorage。无框架依赖。
- **Create** `frontend/src/utils/__tests__/columnOrder.spec.ts` — util 单测。
- **Create** `frontend/src/components/common/__tests__/DataTable.reorder.spec.ts` — 组件渲染单测（顺序应用、手柄存在性、`reorderable=false`）。
- **Modify** `frontend/src/components/common/DataTable.vue` — 接入 util + 拖拽 + 手柄 UI + 持久化。
- **Modify** `frontend/src/i18n/locales/en.ts` 与 `frontend/src/i18n/locales/zh.ts` — 新增手柄 a11y 文案 key。
- **不改** `types.ts`（按 key 自动识别 select/actions，无需新增 `Column` 字段）。**不改** 任何调用页。

设计依据：`docs/specs/2026-06-03-datatable-column-reorder-design.md`。

---

### Task 1: `applyColumnOrder` 纯函数

**Files:**
- Create: `frontend/src/utils/columnOrder.ts`
- Test: `frontend/src/utils/__tests__/columnOrder.spec.ts`

`applyColumnOrder(columns, order)` 规则：把首列 `select`（仅当它确实是第 0 列）钉在最前、`actions` 列钉在最后；中间「可移动列」按 `order`（列 key 数组）排；`order` 中已不存在的 key 忽略；`columns` 中存在但不在 `order` 里的新列，按其在 `columns` 中的原始顺序追加到中间组末尾。

- [ ] **Step 1: 写失败测试**

```ts
// frontend/src/utils/__tests__/columnOrder.spec.ts
import { describe, expect, it } from 'vitest'
import { applyColumnOrder } from '@/utils/columnOrder'
import type { Column } from '@/components/common/types'

const cols = (...keys: string[]): Column[] => keys.map((key) => ({ key, label: key }))

describe('applyColumnOrder', () => {
  it('returns columns unchanged when order is empty', () => {
    const input = cols('name', 'status', 'usage')
    expect(applyColumnOrder(input, []).map((c) => c.key)).toEqual(['name', 'status', 'usage'])
  })

  it('reorders middle columns by the saved order', () => {
    const input = cols('name', 'status', 'usage')
    expect(applyColumnOrder(input, ['usage', 'name', 'status']).map((c) => c.key)).toEqual([
      'usage', 'name', 'status',
    ])
  })

  it('keeps leading select column pinned first', () => {
    const input = cols('select', 'name', 'status')
    expect(applyColumnOrder(input, ['status', 'name', 'select']).map((c) => c.key)).toEqual([
      'select', 'status', 'name',
    ])
  })

  it('keeps actions column pinned last', () => {
    const input = cols('name', 'status', 'actions')
    expect(applyColumnOrder(input, ['actions', 'status', 'name']).map((c) => c.key)).toEqual([
      'status', 'name', 'actions',
    ])
  })

  it('pins both select (first) and actions (last)', () => {
    const input = cols('select', 'name', 'status', 'actions')
    expect(applyColumnOrder(input, ['status', 'name']).map((c) => c.key)).toEqual([
      'select', 'status', 'name', 'actions',
    ])
  })

  it('ignores saved keys that no longer exist', () => {
    const input = cols('name', 'status')
    expect(applyColumnOrder(input, ['gone', 'status', 'name']).map((c) => c.key)).toEqual([
      'status', 'name',
    ])
  })

  it('appends new (unsaved) columns at the end of the middle group in original order', () => {
    const input = cols('name', 'status', 'newA', 'newB')
    expect(applyColumnOrder(input, ['status', 'name']).map((c) => c.key)).toEqual([
      'status', 'name', 'newA', 'newB',
    ])
  })

  it('does not treat a non-first select as pinned', () => {
    const input = cols('name', 'select')
    expect(applyColumnOrder(input, ['select', 'name']).map((c) => c.key)).toEqual([
      'select', 'name',
    ])
  })
})
```

- [ ] **Step 2: 运行确认失败**

Run: `pnpm test:run src/utils/__tests__/columnOrder.spec.ts`
Expected: FAIL — `Failed to resolve import "@/utils/columnOrder"` / `applyColumnOrder is not a function`.

- [ ] **Step 3: 实现**

```ts
// frontend/src/utils/columnOrder.ts
import type { Column } from '@/components/common/types'

/**
 * Apply a saved column order to a set of columns.
 * - A leading `select` column (only when it is index 0) stays pinned first.
 * - An `actions` column stays pinned last.
 * - Remaining ("movable") columns are ordered by `order` (array of column keys);
 *   keys in `order` that no longer exist are ignored, and columns not present in
 *   `order` are appended at the end of the movable group in their original order.
 */
export function applyColumnOrder(columns: Column[], order: string[]): Column[] {
  const leading: Column[] = []
  const trailing: Column[] = []
  const middle: Column[] = []

  columns.forEach((col, index) => {
    if (index === 0 && col.key === 'select') {
      leading.push(col)
    } else if (col.key === 'actions') {
      trailing.push(col)
    } else {
      middle.push(col)
    }
  })

  const byKey = new Map(middle.map((col) => [col.key, col]))
  const ordered: Column[] = []
  for (const key of order) {
    const col = byKey.get(key)
    if (col) {
      ordered.push(col)
      byKey.delete(key)
    }
  }
  for (const col of middle) {
    if (byKey.has(col.key)) ordered.push(col)
  }

  return [...leading, ...ordered, ...trailing]
}
```

- [ ] **Step 4: 运行确认通过**

Run: `pnpm test:run src/utils/__tests__/columnOrder.spec.ts`
Expected: PASS (8 passed).

- [ ] **Step 5: Commit**

```bash
git add frontend/src/utils/columnOrder.ts frontend/src/utils/__tests__/columnOrder.spec.ts
git commit -m "feat(datatable): add applyColumnOrder helper

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: localStorage 读写 + key 构造

**Files:**
- Modify: `frontend/src/utils/columnOrder.ts`
- Test: `frontend/src/utils/__tests__/columnOrder.spec.ts`

- [ ] **Step 1: 追加失败测试**（加到同一个 spec 文件，新增 import 与 describe）

把第一行 import 改为：

```ts
import {
  applyColumnOrder,
  buildColumnOrderStorageKey,
  readColumnOrder,
  writeColumnOrder,
} from '@/utils/columnOrder'
```

在文件末尾追加：

```ts
describe('column order storage', () => {
  afterEach(() => localStorage.clear())

  it('builds a namespaced storage key', () => {
    expect(buildColumnOrderStorageKey('AdminUsers')).toBe('s2a:colorder:AdminUsers')
  })

  it('writes and reads back an order array', () => {
    const key = buildColumnOrderStorageKey('t1')
    writeColumnOrder(key, ['a', 'b', 'c'])
    expect(readColumnOrder(key)).toEqual(['a', 'b', 'c'])
  })

  it('returns [] when nothing is stored', () => {
    expect(readColumnOrder('s2a:colorder:missing')).toEqual([])
  })

  it('returns [] and does not throw on corrupted json', () => {
    localStorage.setItem('s2a:colorder:bad', '{not json')
    expect(readColumnOrder('s2a:colorder:bad')).toEqual([])
  })

  it('filters non-string entries out of stored arrays', () => {
    localStorage.setItem('s2a:colorder:mixed', JSON.stringify(['a', 1, null, 'b']))
    expect(readColumnOrder('s2a:colorder:mixed')).toEqual(['a', 'b'])
  })
})
```

并把顶部 vitest import 补上 `afterEach`：

```ts
import { afterEach, describe, expect, it } from 'vitest'
```

- [ ] **Step 2: 运行确认失败**

Run: `pnpm test:run src/utils/__tests__/columnOrder.spec.ts`
Expected: FAIL — `buildColumnOrderStorageKey is not a function`.

- [ ] **Step 3: 实现**（追加到 `columnOrder.ts` 末尾）

```ts
const STORAGE_PREFIX = 's2a:colorder:'

export function buildColumnOrderStorageKey(id: string): string {
  return `${STORAGE_PREFIX}${id}`
}

export function readColumnOrder(storageKey: string): string[] {
  try {
    const raw = localStorage.getItem(storageKey)
    if (!raw) return []
    const parsed = JSON.parse(raw)
    if (!Array.isArray(parsed)) return []
    return parsed.filter((entry): entry is string => typeof entry === 'string')
  } catch (e) {
    console.error('[columnOrder] Failed to read column order:', e)
    return []
  }
}

export function writeColumnOrder(storageKey: string, order: string[]): void {
  try {
    localStorage.setItem(storageKey, JSON.stringify(order))
  } catch (e) {
    console.error('[columnOrder] Failed to persist column order:', e)
  }
}
```

- [ ] **Step 4: 运行确认通过**

Run: `pnpm test:run src/utils/__tests__/columnOrder.spec.ts`
Expected: PASS (13 passed).

- [ ] **Step 5: Commit**

```bash
git add frontend/src/utils/columnOrder.ts frontend/src/utils/__tests__/columnOrder.spec.ts
git commit -m "feat(datatable): add column-order storage helpers

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: DataTable 应用保存的列序（不含拖拽）

先让 `DataTable.vue` 用 `renderColumns` 渲染并应用持久化顺序。这一步行为对用户不可见（无保存顺序时 = 原顺序），但把渲染基础切好，便于下一步加拖拽。

**Files:**
- Modify: `frontend/src/components/common/DataTable.vue`
- Test: `frontend/src/components/common/__tests__/DataTable.reorder.spec.ts`

- [ ] **Step 1: 写组件失败测试**

```ts
// frontend/src/components/common/__tests__/DataTable.reorder.spec.ts
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { ref } from 'vue'
import DataTable from '../DataTable.vue'
import type { Column } from '../types'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key, locale: ref('en') }),
}))

// DataTable calls window.matchMedia on mount; jsdom lacks it.
beforeEach(() => {
  window.matchMedia = vi.fn().mockImplementation((query: string) => ({
    matches: true, // force desktop <table> render (not mobile cards)
    media: query,
    onchange: null,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    addListener: vi.fn(),
    removeListener: vi.fn(),
    dispatchEvent: vi.fn(),
  })) as unknown as typeof window.matchMedia
})

afterEach(() => {
  localStorage.clear()
  vi.restoreAllMocks()
})

const columns: Column[] = [
  { key: 'name', label: 'Name' },
  { key: 'status', label: 'Status' },
  { key: 'usage', label: 'Usage' },
  { key: 'actions', label: 'Actions' },
]
const data = [{ id: 1, name: 'a', status: 'ok', usage: 1 }]

const headerKeys = (wrapper: ReturnType<typeof mount>) =>
  wrapper.findAll('thead th').map((th) => th.attributes('data-col-key'))

const mountTable = (props: Record<string, unknown> = {}) =>
  mount(DataTable, {
    props: { columns, data, tableId: 'TestTable', ...props },
    global: { stubs: { Icon: true } },
  })

describe('DataTable column order', () => {
  it('renders columns in natural order when nothing is persisted', () => {
    const wrapper = mountTable()
    expect(headerKeys(wrapper)).toEqual(['name', 'status', 'usage', 'actions'])
  })

  it('applies a persisted order, keeping actions pinned last', () => {
    localStorage.setItem('s2a:colorder:TestTable', JSON.stringify(['usage', 'name', 'status']))
    const wrapper = mountTable()
    expect(headerKeys(wrapper)).toEqual(['usage', 'name', 'status', 'actions'])
  })

  it('ignores persisted order when reorderable is false', () => {
    localStorage.setItem('s2a:colorder:TestTable', JSON.stringify(['usage', 'name', 'status']))
    const wrapper = mountTable({ reorderable: false })
    expect(headerKeys(wrapper)).toEqual(['name', 'status', 'usage', 'actions'])
  })
})
```

- [ ] **Step 2: 运行确认失败**

Run: `pnpm test:run src/components/common/__tests__/DataTable.reorder.spec.ts`
Expected: FAIL — `data-col-key` 属性不存在（`headerKeys` 返回 `[undefined, ...]`），第二个用例顺序不符。

- [ ] **Step 3: 改 `DataTable.vue` — 脚本部分**

3a. 顶部 import 增加 `useRoute` 与 util（放在现有 import 区，约第 199–203 行附近）：

```ts
import { useRoute } from 'vue-router'
import {
  applyColumnOrder,
  buildColumnOrderStorageKey,
  readColumnOrder,
} from '@/utils/columnOrder'
```

3b. 在 `interface Props` 中追加两个可选字段（加到 `Props` 内现有字段之后）：

```ts
  /** Enable drag-to-reorder of column headers (default true). */
  reorderable?: boolean
  /** Explicit persistence id for column order; defaults to the route name/path. */
  tableId?: string
```

3c. 在 `withDefaults(defineProps<Props>(), { ... })` 的默认值对象里追加：

```ts
  reorderable: true,
```

3d. 在 `const props = withDefaults(...)` 之后、`const sortKey = ref(...)` 附近，新增列序状态与构建逻辑：

```ts
const route = useRoute() // undefined when no router is installed (e.g. unit tests)

const columnOrderStorageKey = computed<string | null>(() => {
  if (!props.reorderable) return null
  if (props.tableId) return buildColumnOrderStorageKey(props.tableId)
  const id = route?.name ? String(route.name) : route?.path
  return id ? buildColumnOrderStorageKey(id) : null
})

const columnOrder = ref<string[]>([])
const renderColumns = ref<Column[]>([])

const rebuildRenderColumns = () => {
  renderColumns.value = props.reorderable
    ? applyColumnOrder(props.columns, columnOrder.value)
    : [...props.columns]
}

// Build synchronously during setup so first paint already reflects saved order.
if (columnOrderStorageKey.value) {
  columnOrder.value = readColumnOrder(columnOrderStorageKey.value)
}
rebuildRenderColumns()

watch(() => props.columns, rebuildRenderColumns)
```

3e. 把 `dataColumns` 改为基于 `renderColumns`（现约第 505 行）：

```ts
const dataColumns = computed(() => renderColumns.value.filter((column) => column.key !== 'actions'))
```

- [ ] **Step 4: 改 `DataTable.vue` — 模板部分**

把模板里渲染列的循环从 `columns` 切到 `renderColumns`，并给桌面表头 `<th>` 加 `data-col-key`：

4a. 桌面表头循环（现约第 76 行）：

```html
          <th
            v-for="(column, index) in renderColumns"
            :key="column.key"
            scope="col"
            :data-col-key="column.key"
```

4b. 桌面 loading 骨架行（现约第 124 行）：`v-for="column in columns"` → `v-for="column in renderColumns"`。

4c. 桌面空状态 colspan（现约第 134 行）：`:colspan="columns.length"` → `:colspan="renderColumns.length"`。

4d. 桌面虚拟行：上 padding colspan（约 155）、单元格循环 `v-for="(column, colIndex) in columns"`（约 168）、下 padding colspan（约 188）——三处 `columns` 全改为 `renderColumns`。

> 说明：`dataColumns`（移动端卡片、移动端骨架）已在 3e 改为派生自 `renderColumns`，模板里 `dataColumns` 引用无需改动。`hasSelectColumn` / `hasActionsColumn` / `columnsSignature` 仍基于 `props.columns`（集合不变），保持原样——列重排不会误触发排序重置。

- [ ] **Step 5: 运行确认通过**

Run: `pnpm test:run src/components/common/__tests__/DataTable.reorder.spec.ts`
Expected: PASS (3 passed)。

- [ ] **Step 6: Commit**

```bash
git add frontend/src/components/common/DataTable.vue frontend/src/components/common/__tests__/DataTable.reorder.spec.ts
git commit -m "feat(datatable): render via renderColumns and apply persisted order

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: 拖动手柄 + useDraggable + 拖动后持久化

**Files:**
- Modify: `frontend/src/components/common/DataTable.vue`
- Test: `frontend/src/components/common/__tests__/DataTable.reorder.spec.ts`

实现要点：`useDraggable(headerRowRef, renderColumns, options)`，`draggable: 'th'` 让列表索引与 `renderColumns` 1:1 对应；`handle: '.col-drag-handle'` 仅手柄可拖；`filter: '.col-pinned'` + `onMove` 拒绝越过钉死列，保证 `select` 恒在首、`actions` 恒在尾。拖动结束后把 `renderColumns` 中可移动列的 key 写回 `columnOrder` 并持久化。

- [ ] **Step 1: 追加失败测试**（加到 `DataTable.reorder.spec.ts` 的 describe 内）

```ts
  it('shows a drag handle on movable columns but not on pinned (actions) or when disabled', () => {
    const wrapper = mountTable()
    const movable = wrapper.find('thead th[data-col-key="name"]')
    const actions = wrapper.find('thead th[data-col-key="actions"]')
    expect(movable.find('.col-drag-handle').exists()).toBe(true)
    expect(actions.find('.col-drag-handle').exists()).toBe(false)

    const disabled = mountTable({ reorderable: false })
    expect(disabled.find('.col-drag-handle').exists()).toBe(false)
  })

  it('marks pinned columns with col-pinned and movable with col-movable', () => {
    const wrapper = mountTable()
    expect(wrapper.find('thead th[data-col-key="actions"]').classes()).toContain('col-pinned')
    expect(wrapper.find('thead th[data-col-key="name"]').classes()).toContain('col-movable')
  })
```

> 备注：真实拖拽依赖 Sortable 的原生 DOM 事件，jsdom 无法可靠模拟，**拖拽落位与持久化由 QA 手动验证**（见设计文档测试章节）。单测覆盖手柄渲染、钉死/可移动标记、以及 Task 1/2 的重排与持久化纯逻辑。

- [ ] **Step 2: 运行确认失败**

Run: `pnpm test:run src/components/common/__tests__/DataTable.reorder.spec.ts`
Expected: FAIL — `.col-drag-handle` / `col-movable` 不存在。

- [ ] **Step 3: 脚本 — 引入 useDraggable 与持久化回写**

3a. 顶部 import 增加（与 Task 3 的 import 同区）：

```ts
import { useDraggable } from 'vue-draggable-plus'
import { writeColumnOrder } from '@/utils/columnOrder'
```

> 注意：Task 2 已从 `@/utils/columnOrder` 引入 `applyColumnOrder` 等；把 `writeColumnOrder` 合并进那条 import 即可，避免重复 import 同一模块。

3b. 在 3d 的状态块之后，新增手柄判定、表头行 ref、拖拽与回写：

```ts
const headerRowRef = ref<HTMLElement | null>(null)

const isMovableColumn = (column: Column) => {
  if (column.key === 'actions') return false
  if (hasSelectColumn.value && column.key === 'select') return false
  return true
}

const persistColumnOrderFromRender = () => {
  const movableKeys = renderColumns.value.filter(isMovableColumn).map((column) => column.key)
  columnOrder.value = movableKeys
  const storageKey = columnOrderStorageKey.value
  if (storageKey) writeColumnOrder(storageKey, movableKeys)
}

useDraggable(headerRowRef, renderColumns, {
  handle: '.col-drag-handle',
  draggable: 'th',
  filter: '.col-pinned',
  animation: 150,
  onMove: (event: any) => {
    // Never let a movable column cross a pinned (select/actions) column.
    if (event?.related?.classList?.contains('col-pinned')) return false
    return true
  },
  onUpdate: () => {
    persistColumnOrderFromRender()
  },
})
```

> `useDraggable` 接受 ref 形式的 `el`，会在 `headerRowRef` 挂载后自动绑定；`reorderable=false` 时模板不渲染手柄（见 3c/4），且无 `.col-drag-handle` 可拖，拖拽实际被禁用。`isMovableColumn` 依赖 `hasSelectColumn`，确保其定义在前（`hasSelectColumn` 现约第 606 行；本块加在脚本靠后位置即可，Vue `<script setup>` 内顶层 const 可互相引用，因 `isMovableColumn` 在调用时才读取 `hasSelectColumn.value`）。

- [ ] **Step 4: 模板 — 表头行 ref + 列类名 + 手柄**

4a. 给桌面表头 `<tr>`（现约第 74 行）加 ref：

```html
      <thead class="table-header bg-gray-50 dark:bg-dark-800">
        <tr ref="headerRowRef">
```

4b. 在 4a-Task3 改过的桌面表头 `<th>` 上追加动态 class（与现有 class 数组合并）。把该 `<th>` 的 `:class="[ ... ]"` 数组末尾加入两项：

```js
              isMovableColumn(column) ? 'col-movable' : 'col-pinned',
              { 'cursor-move': isMovableColumn(column) }
```

4c. 在该 `<th>` 内、`<slot :name="`header-${column.key}`" ...>` 默认内容的 `<div class="flex items-center space-x-1">` 里，最前面插入手柄（仅可移动列且开启时显示）：

```html
              <div class="flex items-center space-x-1">
                <span
                  v-if="reorderable && isMovableColumn(column)"
                  class="col-drag-handle"
                  :title="t('common.dragToReorderColumn')"
                  :aria-label="t('common.dragToReorderColumn')"
                  @click.stop
                >
                  <svg class="h-3.5 w-3.5" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true">
                    <circle cx="7" cy="5" r="1.5" /><circle cx="13" cy="5" r="1.5" />
                    <circle cx="7" cy="10" r="1.5" /><circle cx="13" cy="10" r="1.5" />
                    <circle cx="7" cy="15" r="1.5" /><circle cx="13" cy="15" r="1.5" />
                  </svg>
                </span>
                <span>{{ column.label }}</span>
```

> `@click.stop` 防止点手柄时误触发该列的排序 `@click`。手柄放在默认插槽内容里；自定义了 `header-<key>` 插槽的调用方不会显示手柄（可接受——这类列通常本就特殊；如需可后续单独处理）。

- [ ] **Step 5: 模板 — 手柄样式（scoped style）**

在 `<style scoped>` 区（现约第 707 行起）追加：

```css
/* 列拖动手柄：默认隐藏，hover/聚焦表头时显现 */
.col-drag-handle {
  display: inline-flex;
  align-items: center;
  margin-right: 2px;
  cursor: grab;
  color: rgb(156 163 175);
  opacity: 0;
  transition: opacity 0.12s ease-in-out;
}
.col-drag-handle:active {
  cursor: grabbing;
}
th:hover .col-drag-handle,
.col-drag-handle:focus-visible {
  opacity: 1;
}
.dark .col-drag-handle {
  color: rgb(107 114 128);
}
```

- [ ] **Step 6: 运行确认通过**

Run: `pnpm test:run src/components/common/__tests__/DataTable.reorder.spec.ts`
Expected: PASS (5 passed)。

- [ ] **Step 7: Commit**

```bash
git add frontend/src/components/common/DataTable.vue frontend/src/components/common/__tests__/DataTable.reorder.spec.ts
git commit -m "feat(datatable): drag handle + useDraggable column reordering

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: i18n 文案

**Files:**
- Modify: `frontend/src/i18n/locales/en.ts`
- Modify: `frontend/src/i18n/locales/zh.ts`

- [ ] **Step 1: en.ts**

在 `common: {` 对象内（约第 251 行起，与 `actions: 'Actions'` 同级）新增一行：

```ts
    dragToReorderColumn: 'Drag to reorder column',
```

- [ ] **Step 2: zh.ts**

在 zh.ts 对应的 `common: {` 对象内新增一行（与同级其他 key 缩进一致）：

```ts
    dragToReorderColumn: '拖动调整列顺序',
```

- [ ] **Step 3: 验证 key 解析**

Run: `pnpm test:run src/components/common/__tests__/DataTable.reorder.spec.ts`
Expected: PASS（i18n 已被 mock 为返回 key，本步主要确保未破坏文件；真实文案在 QA/运行时验证）。

- [ ] **Step 4: Commit**

```bash
git add frontend/src/i18n/locales/en.ts frontend/src/i18n/locales/zh.ts
git commit -m "i18n: add dragToReorderColumn label (en/zh)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: 全量校验（typecheck / 测试 / lint）

**Files:** 无新增改动；仅校验与可能的小修。

- [ ] **Step 1: 类型检查**

Run: `pnpm typecheck`
Expected: 无错误。若 `onMove`/`event` 报类型，确认 `event` 用 `any`（Sortable 的 `MoveEvent` 未从包导出，`any` 可接受）。

- [ ] **Step 2: 全量单测**

Run: `pnpm test:run`
Expected: 全绿（含新增的 columnOrder 与 DataTable.reorder 用例）。

- [ ] **Step 3: Lint**

Run: `pnpm lint:check`
Expected: 无 error。若有，按提示修正（如未使用变量、import 顺序）。

- [ ] **Step 4: Commit（若 Step 1/3 有修正）**

```bash
git add -A
git commit -m "chore(datatable): satisfy typecheck and lint for column reorder

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## QA（实现后人工验证，逐页）

对 18 个表格页面（清单见设计文档）逐一验证：

1. 表头 hover 出现手柄；从手柄拖动可重排可移动列。
2. 刷新页面后列序保留（localStorage）。
3. 点击可排序表头仍正常排序（与拖动互不干扰）。
4. `select`（勾选）与 `actions`（操作）列始终分别在首/尾，不可拖、不被越过。
5. 暗色模式手柄可见、样式正常。
6. 移动端（窄屏）卡片视图按保存顺序展示，且无拖动。

重点抽查带勾选列的页面（如 `/admin/users`、`/admin/accounts`、`/admin/channels`）与共用组件页面（`/admin/usage` vs `/usage`、`/orders` vs admin 订单、三个 `/admin/affiliates/*`）。

## Cherry-pick 回流（QA 通过后）

`columnOrder.ts`(新增) 与 `DataTable.vue`/`i18n` 改动均不依赖 fork 专属代码；从 `upstream/main` 拉分支后 cherry-pick 本计划产生的 commit 即可提 PR。冲突预期为零（核心文件与上游字节级一致）。
