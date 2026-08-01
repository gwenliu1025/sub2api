import { describe, expect, it, vi } from 'vitest'

import type { UpdateAgentStatus } from '@/api/admin/system'
import { waitForUpdateActivation } from '../updateActivation'

function status(state: UpdateAgentStatus['state'], message = ''): UpdateAgentStatus {
  return {
    state,
    current_image: 'ghcr.io/gwenliu1025/sub2api:0.1.169',
    target_image: 'ghcr.io/gwenliu1025/sub2api:0.1.169',
    previous_image: 'ghcr.io/gwenliu1025/sub2api:0.1.156',
    message,
    updated_at: '2026-08-01T00:00:00Z'
  }
}

describe('waitForUpdateActivation', () => {
  it('持续轮询直到代理报告 healthy', async () => {
    const getStatus = vi
      .fn()
      .mockResolvedValueOnce(status('activating'))
      .mockResolvedValueOnce(status('healthy'))

    const result = await waitForUpdateActivation(getStatus, {
      maxAttempts: 3,
      delay: async () => undefined
    })

    expect(result).toEqual({ outcome: 'healthy', status: status('healthy') })
    expect(getStatus).toHaveBeenCalledTimes(2)
  })

  it.each(['rolled_back', 'failed', 'rollback_failed'] as const)(
    '代理报告 %s 时立即返回失败结果',
    async (state) => {
      const failedStatus = status(state, `agent ${state}`)
      const getStatus = vi.fn().mockResolvedValue(failedStatus)

      const result = await waitForUpdateActivation(getStatus, {
        maxAttempts: 3,
        delay: async () => undefined
      })

      expect(result).toEqual({ outcome: 'failed', status: failedStatus })
      expect(getStatus).toHaveBeenCalledTimes(1)
    }
  )

  it('代理暂时不可达时继续轮询', async () => {
    const getStatus = vi
      .fn()
      .mockRejectedValueOnce(new Error('connection reset'))
      .mockResolvedValueOnce(status('healthy'))

    const result = await waitForUpdateActivation(getStatus, {
      maxAttempts: 2,
      delay: async () => undefined
    })

    expect(result.outcome).toBe('healthy')
    expect(getStatus).toHaveBeenCalledTimes(2)
  })

  it('超过轮询次数后返回超时', async () => {
    const getStatus = vi.fn().mockResolvedValue(status('activating'))

    const result = await waitForUpdateActivation(getStatus, {
      maxAttempts: 2,
      delay: async () => undefined
    })

    expect(result).toEqual({ outcome: 'timeout', status: status('activating') })
    expect(getStatus).toHaveBeenCalledTimes(2)
  })
})
