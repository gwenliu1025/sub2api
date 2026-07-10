import { beforeEach, describe, expect, it, vi } from 'vitest'

const { get, post } = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
}))

vi.mock('../client', () => ({
  apiClient: {
    get,
    post,
  },
}))

import {
  getRollbackVersions,
  getUpdateStatus,
  performUpdate,
  restartService,
  rollback,
  type RestartResult,
  type RollbackVersionInfo,
  type UpdateAgentStatus,
  type UpdateMode,
} from '@/api/admin/system'

type Assert<T extends true> = T
type IsExact<T, U> = (
  (<G>() => G extends T ? 1 : 2) extends (<G>() => G extends U ? 1 : 2)
    ? ((<G>() => G extends U ? 1 : 2) extends (<G>() => G extends T ? 1 : 2) ? true : false)
    : false
)

type ExpectedRestartResult = {
  message: string
  update_mode: UpdateMode
  status?: UpdateAgentStatus
}

const restartResultContractExact: Assert<IsExact<RestartResult, ExpectedRestartResult>> = true

describe('admin system rollback API', () => {
  beforeEach(() => {
    get.mockReset()
    post.mockReset()
  })

  it('getRollbackVersions fetches the rollback version list', async () => {
    const versions: RollbackVersionInfo[] = [
      {
        version: '0.1.146',
        published_at: '2026-07-07T00:00:00Z',
        html_url: 'https://github.com/Wei-Shaw/sub2api/releases/tag/v0.1.146'
      }
    ]
    get.mockResolvedValue({ data: { versions } })

    const result = await getRollbackVersions()

    expect(get).toHaveBeenCalledWith('/admin/system/rollback-versions')
    expect(result.versions).toEqual(versions)
  })

  it('rollback posts the target version in the request body', async () => {
    post.mockResolvedValue({ data: { message: 'ok', need_restart: true } })

    const result = await rollback('0.1.146')

    expect(post).toHaveBeenCalledWith('/admin/system/rollback', { version: '0.1.146' })
    expect(result.need_restart).toBe(true)
  })

  it('rollback without a version posts no body (legacy backup rollback)', async () => {
    post.mockResolvedValue({ data: { message: 'ok', need_restart: true } })

    await rollback()

    expect(post).toHaveBeenCalledWith('/admin/system/rollback', undefined)
  })

  it('performUpdate uses the extended update timeout', async () => {
    post.mockResolvedValue({ data: { message: 'ok', need_restart: true } })

    await performUpdate()

    expect(post).toHaveBeenCalledWith('/admin/system/update', undefined, { timeout: 610_000 })
  })

  it('getUpdateStatus fetches the update agent status', async () => {
    const status: UpdateAgentStatus = {
      state: 'activating',
      current_image: 'ghcr.io/example/sub2api:0.1.149',
      target_image: 'ghcr.io/example/sub2api:0.1.150',
      previous_image: 'ghcr.io/example/sub2api:0.1.148',
      message: 'activation started',
      updated_at: '2026-07-10T10:00:00Z',
    }
    get.mockResolvedValue({ data: status })

    const result = await getUpdateStatus()

    expect(get).toHaveBeenCalledWith('/admin/system/update-status')
    expect(result).toEqual(status)
  })

  it('restartService preserves the backend response contract and call', async () => {
    const response: RestartResult = {
      message: 'Image activation initiated',
      update_mode: 'docker_agent',
      status: {
        state: 'activating',
        current_image: 'ghcr.io/example/sub2api:0.1.149',
        target_image: 'ghcr.io/example/sub2api:0.1.150',
        previous_image: 'ghcr.io/example/sub2api:0.1.148',
        message: 'activation started',
        updated_at: '2026-07-10T10:00:00Z',
      },
    }
    post.mockResolvedValue({ data: response })

    const result = await restartService()

    expect(post).toHaveBeenCalledWith('/admin/system/restart')
    expect(result).toEqual(response)
    expect(restartResultContractExact).toBe(true)
  })
})
