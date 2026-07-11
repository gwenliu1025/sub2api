/**
 * System API endpoints for admin operations
 */

import { apiClient } from '../client'

export interface ReleaseInfo {
  name: string
  body: string
  published_at: string
  html_url: string
}

export type UpdateMode = 'binary' | 'docker_agent'

export type UpdateAgentState =
  | 'idle'
  | 'preparing'
  | 'prepared'
  | 'activating'
  | 'healthy'
  | 'rolled_back'
  | 'failed'
  | 'rollback_failed'

export interface UpdateAgentStatus {
  state: UpdateAgentState
  current_image: string
  target_image: string
  previous_image: string
  message: string
  updated_at: string
}

export interface VersionInfo {
  current_version: string
  latest_version: string
  has_update: boolean
  release_info?: ReleaseInfo
  cached: boolean
  warning?: string
  build_type: string // "source" for manual builds, "release" for CI builds
  update_mode: UpdateMode
}

/**
 * Get current version
 */
export async function getVersion(): Promise<{ version: string }> {
  const { data } = await apiClient.get<{ version: string }>('/admin/system/version')
  return data
}

/**
 * Check for updates
 * @param force - Force refresh from GitHub API
 */
export async function checkUpdates(force = false): Promise<VersionInfo> {
  const { data } = await apiClient.get<VersionInfo>('/admin/system/check-updates', {
    params: force ? { force: 'true' } : undefined
  })
  return data
}

export interface UpdateResult {
  message: string
  need_restart: boolean
}

export interface RollbackVersionInfo {
  version: string
  published_at: string
  html_url: string
}

/**
 * Get versions available for rollback (up to 3 versions older than current)
 */
export async function getRollbackVersions(): Promise<{ versions: RollbackVersionInfo[] }> {
  const { data } = await apiClient.get<{ versions: RollbackVersionInfo[] }>(
    '/admin/system/rollback-versions'
  )
  return data
}

/**
 * Perform system update
 * Downloads and applies the latest version
 */
export async function performUpdate(): Promise<UpdateResult> {
  const { data } = await apiClient.post<UpdateResult>('/admin/system/update', undefined, {
    timeout: 610_000
  })
  return data
}

export async function getUpdateStatus(): Promise<UpdateAgentStatus> {
  const { data } = await apiClient.get<UpdateAgentStatus>('/admin/system/update-status')
  return data
}

/**
 * Rollback to a previous version
 * @param version - Target version (e.g. "0.1.146"); omit to restore the local backup binary
 */
export async function rollback(version?: string): Promise<UpdateResult> {
  const { data } = await apiClient.post<UpdateResult>(
    '/admin/system/rollback',
    version ? { version } : undefined
  )
  return data
}

/**
 * Restart the service
 */
export interface RestartResult {
  message: string
  update_mode: UpdateMode
  status?: UpdateAgentStatus
}

export async function restartService(): Promise<RestartResult> {
  const { data } = await apiClient.post<RestartResult>('/admin/system/restart')
  return data
}

export const systemAPI = {
  getVersion,
  checkUpdates,
  performUpdate,
  getUpdateStatus,
  getRollbackVersions,
  rollback,
  restartService
}

export default systemAPI
