import { describe, expect, it, vi } from 'vitest'

import type { UpdateAgentState, UpdateAgentStatus } from '@/api/admin/system'
import { waitForUpdateActivation } from '../updateActivation'

function createStatus(
  state: UpdateAgentState,
  message: string = state,
): UpdateAgentStatus {
  return {
    state,
    current_image: 'ghcr.io/example/sub2api:0.1.149',
    target_image: 'ghcr.io/example/sub2api:0.1.150',
    previous_image: 'ghcr.io/example/sub2api:0.1.148',
    message,
    updated_at: '2026-07-10T10:00:00Z',
  }
}

function createClock() {
  let currentTime = 0
  const sleeps: number[] = []

  return {
    now: () => currentTime,
    sleep: async (milliseconds: number) => {
      sleeps.push(milliseconds)
      currentTime += milliseconds
    },
    sleeps,
  }
}

describe('waitForUpdateActivation', () => {
  it('continues polling when recovery takes longer than eight seconds', async () => {
    const clock = createClock()
    const getStatus = vi.fn(async () =>
      createStatus(getStatus.mock.calls.length < 6 ? 'activating' : 'healthy'),
    )

    const result = await waitForUpdateActivation({
      mode: 'docker_agent',
      checkHealth: async () => true,
      getStatus,
      sleep: clock.sleep,
      now: clock.now,
      intervalMs: 2_000,
    })

    expect(result.state).toBe('healthy')
    expect(getStatus).toHaveBeenCalledTimes(6)
    expect(clock.sleeps).toEqual([2_000, 2_000, 2_000, 2_000, 2_000])
  })

  it('returns healthy only after agent status is healthy', async () => {
    const clock = createClock()
    const activating = createStatus('activating')
    const healthy = createStatus('healthy', 'new container is healthy')
    const getStatus = vi.fn()
      .mockResolvedValueOnce(activating)
      .mockResolvedValueOnce(healthy)

    const result = await waitForUpdateActivation({
      mode: 'docker_agent',
      checkHealth: async () => true,
      getStatus,
      sleep: clock.sleep,
      now: clock.now,
    })

    expect(result).toEqual({ state: 'healthy', status: healthy })
    expect(getStatus).toHaveBeenCalledTimes(2)
  })

  it('returns rolled_back with the agent message', async () => {
    const rolledBack = createStatus('rolled_back', 'restored previous image')

    const result = await waitForUpdateActivation({
      mode: 'docker_agent',
      checkHealth: async () => true,
      getStatus: async () => rolledBack,
    })

    expect(result).toEqual({ state: 'rolled_back', status: rolledBack })
    expect(result.state === 'rolled_back' && result.status.message).toBe('restored previous image')
  })

  it('returns rollback_failed with the agent message', async () => {
    const rollbackFailed = createStatus('rollback_failed', 'previous image did not start')

    const result = await waitForUpdateActivation({
      mode: 'docker_agent',
      checkHealth: async () => true,
      getStatus: async () => rollbackFailed,
    })

    expect(result).toEqual({ state: 'rollback_failed', status: rollbackFailed })
    expect(result.state === 'rollback_failed' && result.status.message).toBe(
      'previous image did not start',
    )
  })

  it('returns rolled_back without waiting for a never-resolving health check', async () => {
    const rolledBack = createStatus('rolled_back', 'restored after failed activation')
    const checkHealth = vi.fn(() => new Promise<boolean>(() => {}))
    const getStatus = vi.fn(async () => rolledBack)

    const resultPromise = waitForUpdateActivation({
      mode: 'docker_agent',
      checkHealth,
      getStatus,
    })

    expect(checkHealth).not.toHaveBeenCalled()
    await expect(resultPromise).resolves.toEqual({ state: 'rolled_back', status: rolledBack })
    expect(getStatus).toHaveBeenCalledTimes(1)
  })

  it('returns rollback_failed without consulting a failing health check', async () => {
    const rollbackFailed = createStatus('rollback_failed', 'rollback container failed')
    const checkHealth = vi.fn(async () => {
      throw new Error('service unavailable')
    })
    const getStatus = vi.fn(async () => rollbackFailed)

    const result = await waitForUpdateActivation({
      mode: 'docker_agent',
      checkHealth,
      getStatus,
    })

    expect(result).toEqual({ state: 'rollback_failed', status: rollbackFailed })
    expect(checkHealth).not.toHaveBeenCalled()
    expect(getStatus).toHaveBeenCalledTimes(1)
  })

  it('returns timeout after 120 seconds', async () => {
    const clock = createClock()
    const checkHealth = vi.fn(async () => false)
    const getStatus = vi.fn(async () => createStatus('activating'))

    const result = await waitForUpdateActivation({
      mode: 'docker_agent',
      checkHealth,
      getStatus,
      sleep: clock.sleep,
      now: clock.now,
    })

    expect(result).toEqual({ state: 'timeout' })
    expect(clock.now()).toBe(120_000)
    expect(checkHealth).not.toHaveBeenCalled()
    expect(getStatus).toHaveBeenCalledTimes(60)
  })

  it('returns timeout when docker status never resolves', async () => {
    vi.useFakeTimers({ now: 0 })
    try {
      let outcome: Awaited<ReturnType<typeof waitForUpdateActivation>> | undefined
      const resultPromise = waitForUpdateActivation({
        mode: 'docker_agent',
        checkHealth: vi.fn(),
        getStatus: () => new Promise<UpdateAgentStatus>(() => {}),
        now: () => Date.now(),
        timeoutMs: 50,
        intervalMs: 10,
      })
      void resultPromise.then((result) => {
        outcome = result
      })

      await vi.advanceTimersByTimeAsync(50)

      expect(outcome).toEqual({ state: 'timeout' })
      await expect(resultPromise).resolves.toEqual({ state: 'timeout' })
      expect(vi.getTimerCount()).toBe(0)
    } finally {
      vi.useRealTimers()
    }
  })

  it('returns timeout when binary health never resolves', async () => {
    vi.useFakeTimers({ now: 0 })
    try {
      let outcome: Awaited<ReturnType<typeof waitForUpdateActivation>> | undefined
      const resultPromise = waitForUpdateActivation({
        mode: 'binary',
        checkHealth: () => new Promise<boolean>(() => {}),
        getStatus: vi.fn(),
        now: () => Date.now(),
        timeoutMs: 50,
        intervalMs: 10,
      })
      void resultPromise.then((result) => {
        outcome = result
      })

      await vi.advanceTimersByTimeAsync(50)

      expect(outcome).toEqual({ state: 'timeout' })
      await expect(resultPromise).resolves.toEqual({ state: 'timeout' })
      expect(vi.getTimerCount()).toBe(0)
    } finally {
      vi.useRealTimers()
    }
  })

  it('returns timeout when sleep never resolves', async () => {
    vi.useFakeTimers({ now: 0 })
    try {
      let outcome: Awaited<ReturnType<typeof waitForUpdateActivation>> | undefined
      const resultPromise = waitForUpdateActivation({
        mode: 'docker_agent',
        checkHealth: vi.fn(),
        getStatus: async () => createStatus('activating'),
        sleep: () => new Promise<void>(() => {}),
        now: () => Date.now(),
        timeoutMs: 50,
        intervalMs: 10,
      })
      void resultPromise.then((result) => {
        outcome = result
      })

      await vi.advanceTimersByTimeAsync(50)

      expect(outcome).toEqual({ state: 'timeout' })
      await expect(resultPromise).resolves.toEqual({ state: 'timeout' })
      expect(vi.getTimerCount()).toBe(0)
    } finally {
      vi.useRealTimers()
    }
  })

  it('legacy mode reloads only after health disappears and returns', async () => {
    const clock = createClock()
    const checkHealth = vi.fn()
      .mockResolvedValueOnce(true)
      .mockResolvedValueOnce(false)
      .mockResolvedValueOnce(true)
    const getStatus = vi.fn()

    const result = await waitForUpdateActivation({
      mode: 'binary',
      checkHealth,
      getStatus,
      sleep: clock.sleep,
      now: clock.now,
    })

    expect(result).toEqual({ state: 'healthy' })
    expect(checkHealth).toHaveBeenCalledTimes(3)
    expect(getStatus).not.toHaveBeenCalled()
  })

  it('returns failed with the terminal agent status', async () => {
    const failed = createStatus('failed', 'new image failed to start')

    const result = await waitForUpdateActivation({
      mode: 'docker_agent',
      checkHealth: async () => true,
      getStatus: async () => failed,
    })

    expect(result).toEqual({ state: 'failed', status: failed })
  })

  it('retries transient status errors until the agent is healthy', async () => {
    const clock = createClock()
    const checkHealth = vi.fn()
    const getStatus = vi.fn()
      .mockRejectedValueOnce(new Error('authentication unavailable'))
      .mockResolvedValueOnce(createStatus('activating'))
      .mockResolvedValueOnce(createStatus('healthy'))

    const result = await waitForUpdateActivation({
      mode: 'docker_agent',
      checkHealth,
      getStatus,
      sleep: clock.sleep,
      now: clock.now,
    })

    expect(result.state).toBe('healthy')
    expect(checkHealth).not.toHaveBeenCalled()
    expect(getStatus).toHaveBeenCalledTimes(3)
  })

  it('retries a transient legacy health error until health returns', async () => {
    const clock = createClock()
    const checkHealth = vi.fn()
      .mockRejectedValueOnce(new Error('connection reset'))
      .mockResolvedValueOnce(true)
    const getStatus = vi.fn()

    const result = await waitForUpdateActivation({
      mode: 'binary',
      checkHealth,
      getStatus,
      sleep: clock.sleep,
      now: clock.now,
    })

    expect(result).toEqual({ state: 'healthy' })
    expect(checkHealth).toHaveBeenCalledTimes(2)
    expect(getStatus).not.toHaveBeenCalled()
  })

  it('allows legacy success after the two second grace period', async () => {
    const clock = createClock()
    const checkHealth = vi.fn(async () => true)

    const result = await waitForUpdateActivation({
      mode: 'binary',
      checkHealth,
      getStatus: vi.fn(),
      sleep: clock.sleep,
      now: clock.now,
    })

    expect(result).toEqual({ state: 'healthy' })
    expect(checkHealth).toHaveBeenCalledTimes(2)
    expect(clock.now()).toBe(2_000)
  })

  it('caps the final sleep at the deadline', async () => {
    const clock = createClock()
    const checkHealth = vi.fn(async () => false)

    const result = await waitForUpdateActivation({
      mode: 'binary',
      checkHealth,
      getStatus: vi.fn(),
      sleep: clock.sleep,
      now: clock.now,
      timeoutMs: 2_500,
      intervalMs: 2_000,
    })

    expect(result).toEqual({ state: 'timeout' })
    expect(clock.sleeps).toEqual([2_000, 500])
    expect(checkHealth).toHaveBeenCalledTimes(2)
  })

  it.each([0, -1, Number.NaN, Number.POSITIVE_INFINITY])(
    'rejects invalid timeoutMs %s',
    async (timeoutMs) => {
      await expect(
        waitForUpdateActivation({
          mode: 'docker_agent',
          checkHealth: vi.fn(),
          getStatus: async () => createStatus('healthy'),
          timeoutMs,
        }),
      ).rejects.toThrow(new RangeError('timeoutMs must be a finite positive number'))
    },
  )

  it.each([0, -1, Number.NaN, Number.POSITIVE_INFINITY])(
    'rejects invalid intervalMs %s',
    async (intervalMs) => {
      await expect(
        waitForUpdateActivation({
          mode: 'docker_agent',
          checkHealth: vi.fn(),
          getStatus: async () => createStatus('healthy'),
          intervalMs,
        }),
      ).rejects.toThrow(new RangeError('intervalMs must be a finite positive number'))
    },
  )
})
