import { apiClient } from '../client'

// === Types ===

export interface PayloadAuditExportKey {
  id: string
  name: string
  rate_limit_per_min: number
  created_at: string
  last_used_at?: string
  disabled: boolean
}

export interface PayloadAuditConfig {
  all_groups: boolean
  group_ids: number[]
  input_max_bytes: number
  output_max_bytes: number
  excerpt_bytes: number
  retention_days: number
  worker_count: number
  queue_size: number
  queue_max_bytes: number
  batch_size: number
  batch_flush_ms: number
  export_api_keys: PayloadAuditExportKey[]
}

export interface PayloadAuditConfigEnvelope {
  enabled: boolean
  config: PayloadAuditConfig
}

// PayloadAuditLog fields: the handler returns repository.PayloadAuditRow
// which has NO json tags, so Go serialises with PascalCase field names.
export interface PayloadAuditLog {
  ID: string
  CreatedAt: string
  RequestID: string
  UserID?: number | null
  UserEmail: string
  APIKeyID?: number | null
  APIKeyName: string
  GroupID?: number | null
  GroupName: string
  ClientIP: string
  Endpoint: string
  Provider: string
  Model: string
  UpstreamModel: string
  Stream: boolean
  StatusCode: number
  DurationMs: number
  InputExcerpt: string
  OutputExcerpt: string
  InputBody: string
  OutputBody: string
  InputFormat: string
  OutputFormat: string
  InputBytes: number
  OutputBytes: number
  InputTruncated: boolean
  OutputTruncated: boolean
  OutputOmitted: boolean
  ErrorMessage: string
}

export interface PayloadAuditListResponse {
  data: PayloadAuditLog[]
  next_cursor: string
}

// SinkStats has no json tags → PascalCase
export interface PayloadAuditSinkStats {
  Accepted: number
  DropQueueFull: number
  DropByteBudget: number
  BatchInserted: number
  BatchFailed: number
  WorkerPanic: number
  QueueDepth: number
  QueueBytesUsed: number
  DropOnShutdown: number
}

export interface PayloadAuditStatus {
  enabled: boolean
  workers: { configured: number }
  queue: {
    size: number
    depth: number
    usage_pct: number
    bytes_used: number
    bytes_max: number
  }
  stats_24h: PayloadAuditSinkStats
}

export interface ListPayloadsParams {
  from: string // RFC3339
  to: string
  user_id?: number
  group_id?: number
  api_key_id?: number
  cursor?: string
  limit?: number
  include_body?: 'none' | 'excerpt' | 'full'
  keyword?: string
}

export interface CreateExportKeyResponse {
  token: string
  key: PayloadAuditExportKey
}

// === API ===

export async function getConfig(): Promise<PayloadAuditConfigEnvelope> {
  const { data } = await apiClient.get<PayloadAuditConfigEnvelope>('/admin/payload-audit/config')
  return data
}

export async function updateConfig(
  payload: PayloadAuditConfigEnvelope
): Promise<{ need_rebuild_sink: boolean }> {
  const { data } = await apiClient.put<{ need_rebuild_sink: boolean }>(
    '/admin/payload-audit/config',
    payload
  )
  return data
}

export async function getStatus(): Promise<PayloadAuditStatus> {
  const { data } = await apiClient.get<PayloadAuditStatus>('/admin/payload-audit/status')
  return data
}

export async function listPayloads(
  params: ListPayloadsParams
): Promise<PayloadAuditListResponse> {
  const { data } = await apiClient.get<PayloadAuditListResponse>(
    '/admin/payload-audit/payloads',
    { params }
  )
  return data
}

export async function getPayload(id: string, createdAt: string): Promise<PayloadAuditLog> {
  const { data } = await apiClient.get<PayloadAuditLog>(
    `/admin/payload-audit/payloads/${id}`,
    { params: { created_at: createdAt } }
  )
  return data
}

export async function listExportKeys(): Promise<PayloadAuditExportKey[]> {
  const { data } = await apiClient.get<PayloadAuditExportKey[]>(
    '/admin/payload-audit/export-keys'
  )
  return data
}

export async function createExportKey(
  name: string,
  ratePerMin: number
): Promise<CreateExportKeyResponse> {
  const { data } = await apiClient.post<CreateExportKeyResponse>(
    '/admin/payload-audit/export-keys',
    { name, rate_limit_per_min: ratePerMin }
  )
  return data
}

export async function deleteExportKey(id: string): Promise<void> {
  await apiClient.delete(`/admin/payload-audit/export-keys/${id}`)
}

export async function runCleanup(): Promise<{ deleted: number; duration_ms: number }> {
  const { data } = await apiClient.post<{ deleted: number; duration_ms: number }>(
    '/admin/payload-audit/cleanup'
  )
  return data
}

export const payloadAuditAPI = {
  getConfig,
  updateConfig,
  getStatus,
  listPayloads,
  getPayload,
  listExportKeys,
  createExportKey,
  deleteExportKey,
  runCleanup,
}

export default payloadAuditAPI
