# DataTable 可拖动列序 — 设计文档

- **日期**: 2026-06-03
- **状态**: 已批准，待写实现计划
- **作者**: wood + Claude
- **分支**: `feat/datatable-column-reorder`

## 背景

本仓库 `s2a_for_pcc` 是 `Wei-Shaw/sub2api` 的 fork。前端为 Vue 3 + Vite + TS。
仓库已配置 `upstream` remote，当前领先 `upstream/main` 86 个 commit，但合并基点即最近一次
上游同步（`Merge upstream v0.1.133`）。

关键事实（已核实）：

- 共享表格组件 `frontend/src/components/common/DataTable.vue`、`types.ts`、`tablePreferences.ts`、
  相关 composables 与 `upstream/main` **字节级一致**（fork 从未改动）。
- 拖拽库 `vue-draggable-plus@^0.6.1` 已是 **上游与 fork 共有的依赖**（`GroupsView.vue` 已在别处用它）。
- 18 个表格走 `DataTable.vue`（列由 `columns: Column[]` 驱动，表头/表体/移动端卡片均 `v-for` 该数组，
  插槽按 `column.key` 命名）。另有约 29 个手写 `<table>`，不在本次范围内。

## 目标

为所有走 `DataTable.vue` 的表格（18 个，分布于约 16 个页面）增加「拖动表头重排列顺序」能力，
顺序持久化到 localStorage。**纯前端改动。**

## 非目标（明确不做）

- 手写 `<table>` 的约 29 个表（含 fork 专属的 PayloadAudit / KeyUsage）——日后按需单独处理。
- 列的显示/隐藏（show/hide）。
- 跨设备同步（后端 user-prefs）。
- 「恢复默认列序」重置入口。
- 移动端卡片视图的拖动（仅按保存顺序展示，不可拖）。

## 交付与回流路径

PM 决定：**不先 PR 上游。** 先在 fork 本地实现并通过 QA，再单独 cherry-pick 去给上游提 PR。
因此核心设计约束是：**改动收敛在 `DataTable.vue`（+ `types.ts` + 一个 i18n key）内，调用页零改动**，
使日后的 commit 能干净 cherry-pick 到从 `upstream/main` 拉出的分支。

流程：
1. `feat/datatable-column-reorder` 分支实现 + vitest 单测。
2. QA 手动过 18 个页面。
3. 通过后将（基本独立的）commit cherry-pick 到从 `upstream/main` 拉出的分支 → 给上游提 PR。

## 设计

### 1. 改动边界

- 全部逻辑在 `frontend/src/components/common/DataTable.vue` 内。
- `types.ts` 的 `Column` 视需要加可选标记（见下）。
- 新增一个 i18n key（拖动手柄的 aria-label / title）。
- 18 个调用页 **不改**：功能默认开启。
- 逃生口：新增 prop `reorderable?: boolean`，默认 `true`；将来某表想关闭传 `:reorderable="false"`。

### 2. 拖动交互（专用手柄）

- 可拖动列的 `<th>` 在 hover 时显示 grip 手柄图标（⠿），class `.col-drag-handle`。
- 用 `vue-draggable-plus` 的 `useSortable` 绑定到表头 `<tr>` ref：
  - `handle: '.col-drag-handle'`（只能从手柄拖）。
  - `filter` / `draggable` 配置排除钉死列（见下）。
  - `animation` 适度，拖动时有占位反馈。
- 点击表头其余区域**保持现有排序行为**（`@click="column.sortable && handleSort(column.key)"` 不变）。
  手柄与排序点击互不干扰。

### 3. 钉死列（pinned）

- `select`（首列勾选，`columns[0].key === 'select'`）与 `actions`（末列操作，`key === 'actions'`）：
  - 不显示拖动手柄、不参与拖动、始终保持在两端。
  - sticky 吸附逻辑依赖它们的位置，因此保持不变、不受重排影响。
- 仅「中间的可移动列」参与重排。

### 4. 列序状态与持久化

- 内部计算 `orderedColumns = applyOrder(props.columns, savedOrder)`，渲染处（桌面 `<th>`/`<td>` 两个循环、
  移动端卡片 `dataColumns`）统一改用 `orderedColumns`。
- 父组件传入的 `columns` 仍是权威集合；保存的只是一份「列 key 顺序」（`string[]`）。
- **持久化 key**：
  - 默认按路由自动派生：`s2a:colorder:<routeName>`（在 `DataTable.vue` 内 `useRoute()`）。
  - 可选 prop `tableId?: string` 作为显式覆盖，防同页多表的罕见冲突。
- 存储介质：`localStorage`，值为列 key 数组。读写失败要 try/catch 并降级（照抄现有
  `sortStorageKey` 持久化代码的容错风格）。
- **共用组件跨路由的行为**：`AdminAffiliateRecordsTable`（invites/rebates/transfers 三个路由）、
  `UsageTable`（admin `/admin/usage` 与 user `/usage`）、`OrderTable`（user `/orders` 与 admin 订单）
  在自动 key 方案下**按路由各自独立持久化**（符合预期，因列集可能不同）。若希望某组共享同一份顺序，
  给它们传同一个 `tableId` 即可——但这属于调用页改动，与「零改动、干净 cherry-pick」相悖，v1 默认不做。

### 5. 动态列健壮处理

列是 `computed`，会随权限/数据变。`applyOrder` 规则（照抄现有排序持久化的回退思路）：

- 先抽出钉死列（select / actions），固定到两端。
- 中间列：按 `savedOrder` 排；
  - `savedOrder` 里有、当前 `columns` 没有的 key → 忽略。
  - 当前有、`savedOrder` 里没有的新 key → 追加到其在 `columns` 中的自然相对位置之后（稳定、不丢）。
- 列增删不报错、不丢已保存顺序。

### 6. 其他边界

- **移动端**：卡片视图按 `orderedColumns` 展示，但不绑定拖动。
- **虚拟滚动**：纵向虚拟化，与横向列重排互不影响。
- **暗色模式 / sticky / 阴影**：手柄样式适配暗色；钉死列不动，sticky 行为不变。

## `types.ts` 改动

`Column` 可选新增（具体以实现为准，倾向最小化）：

```ts
export interface Column {
  key: string
  label: string
  sortable?: boolean
  class?: string
  formatter?: (value: any, row: any) => string
  // 可选：标记该列不可拖动（默认可拖，select/actions 自动钉死无需显式标记）
  reorderable?: boolean
}
```

> 备注：是否需要 `Column.reorderable` 取决于实现；若 select/actions 的自动钉死已足够，可不加，
> 保持 `types.ts` 改动最小以利 cherry-pick。实现阶段确认。

## 测试

### 单测（vitest，`components/common/__tests__/`）

- `applyOrder`：保存顺序正确应用。
- 动态列：新增列追加、删除列忽略、不丢顺序。
- 钉死列：select / actions 不参与重排、恒在两端。
- 持久化：读写 localStorage、损坏数据降级。
- `reorderable=false`：关闭后无手柄、不可拖。

### QA 手动

逐一过 18 个表格页面，每个验证：拖动重排 → 刷新后保留 → 排序仍可用 → 暗色正常 → 移动端卡片顺序一致且不可拖。

## 受影响的 18 个表格（页面 / 路由）

| 页面 / 路由 | 表格文件 | fork 改过? |
|---|---|---|
| `/admin/accounts` 上游账号 | `views/admin/AccountsView.vue` | ⚠️ FORK-MOD |
| `/admin/users` 用户管理 | `views/admin/UsersView.vue` | ⚠️ FORK-MOD |
| `/admin/groups` 分组管理 | `views/admin/GroupsView.vue` | 纯净 |
| `/admin/channels` 渠道管理 | `views/admin/ChannelsView.vue` | 纯净 |
| `/admin/channels/monitor` 渠道监控 | `views/admin/ChannelMonitorView.vue` | 纯净 |
| `/admin/subscriptions` 订阅管理 | `views/admin/SubscriptionsView.vue` | 纯净 |
| 促销码管理 | `views/admin/PromoCodesView.vue` | 纯净 |
| `/admin/proxies` 代理管理 | `views/admin/ProxiesView.vue` | 纯净 |
| `/admin/redeem` 兑换码 | `views/admin/RedeemView.vue` | 纯净 |
| `/admin/announcements` 公告管理 + 已读弹窗 | `views/admin/AnnouncementsView.vue` + `components/admin/announcements/AnnouncementReadStatusDialog.vue` | 纯净 |
| 付费套餐 | `views/admin/orders/AdminPaymentPlansView.vue` | 纯净 |
| `/admin/usage` + `/usage` 用量 | `components/admin/usage/UsageTable.vue`（共用） | ⚠️ FORK-MOD |
| `/orders` + admin 订单 | `components/payment/OrderTable.vue`（共用） + `components/admin/payment/AdminOrderTable.vue` | 纯净 |
| `/admin/affiliates/{invites,rebates,transfers}` 推广记录 | `views/admin/affiliates/AdminAffiliateRecordsTable.vue`（共用×3） | 纯净 |
| `/keys` 我的 API Key | `views/user/KeysView.vue` | 纯净 |

> 18 个表格实例分布在约 16 个页面；功能默认开启、调用页零改动，因此 FORK-MOD 文件也无需触碰，
> 合并/cherry-pick 零冲突。

## 待解决问题

无。设计已批准。
