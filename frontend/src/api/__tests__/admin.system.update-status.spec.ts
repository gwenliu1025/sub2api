import { beforeEach, describe, expect, it, vi } from 'vitest'

const { get } = vi.hoisted(() => ({
  get: vi.fn()
}))

vi.mock('../client', () => ({
  apiClient: {
    get
  }
}))

import { getUpdateStatus, systemAPI, type UpdateAgentStatus } from '@/api/admin/system'

describe('admin system update status API', () => {
  beforeEach(() => {
    get.mockReset()
  })

  it('获取更新代理状态并通过 systemAPI 导出', async () => {
    const status: UpdateAgentStatus = {
      state: 'prepared',
      current_image: 'ghcr.io/example/sub2api:current',
      target_image: 'ghcr.io/example/sub2api:target',
      previous_image: 'ghcr.io/example/sub2api:previous',
      message: 'ready',
      updated_at: '2026-08-01T00:00:00Z'
    }
    get.mockResolvedValue({ data: status })

    const result = await getUpdateStatus()

    expect(get).toHaveBeenCalledWith('/admin/system/update-status')
    expect(result).toEqual(status)
    expect(systemAPI.getUpdateStatus).toBe(getUpdateStatus)
  })
})
