import type { UpdateAgentStatus, UpdateMode } from '@/api/admin/system'

export type ActivationOutcome =
  | { state: 'healthy'; status?: UpdateAgentStatus }
  | { state: 'rolled_back'; status: UpdateAgentStatus }
  | { state: 'rollback_failed'; status: UpdateAgentStatus }
  | { state: 'failed'; status?: UpdateAgentStatus }
  | { state: 'timeout' }

export interface WaitForUpdateActivationOptions {
  mode: UpdateMode
  checkHealth: () => Promise<boolean>
  getStatus: () => Promise<UpdateAgentStatus>
  sleep?: (milliseconds: number) => Promise<void>
  now?: () => number
  timeoutMs?: number
  intervalMs?: number
}

const DEFAULT_TIMEOUT_MS = 120_000
const DEFAULT_INTERVAL_MS = 2_000
const LEGACY_HEALTH_GRACE_MS = 2_000

const defaultSleep = (milliseconds: number): Promise<void> =>
  new Promise((resolve) => setTimeout(resolve, milliseconds))

const defaultNow = (): number => {
  if (typeof globalThis.performance?.now === 'function') {
    return globalThis.performance.now()
  }
  return Date.now()
}

type DeadlineResult<T> =
  | { state: 'resolved'; value: T }
  | { state: 'timeout' }

function assertFinitePositive(name: string, value: number): void {
  if (!Number.isFinite(value) || value <= 0) {
    throw new RangeError(`${name} must be a finite positive number`)
  }
}

async function awaitBeforeDeadline<T>(
  operation: () => Promise<T>,
  deadline: number,
  now: () => number,
): Promise<DeadlineResult<T>> {
  const remainingMs = deadline - now()
  if (remainingMs <= 0) {
    return { state: 'timeout' }
  }

  let deadlineTimer: ReturnType<typeof setTimeout> | undefined
  try {
    return await Promise.race([
      Promise.resolve()
        .then(operation)
        .then((value): DeadlineResult<T> => ({ state: 'resolved', value })),
      new Promise<DeadlineResult<T>>((resolve) => {
        deadlineTimer = setTimeout(() => resolve({ state: 'timeout' }), remainingMs)
      }),
    ])
  } finally {
    if (deadlineTimer !== undefined) {
      clearTimeout(deadlineTimer)
    }
  }
}

export async function waitForUpdateActivation({
  mode,
  checkHealth,
  getStatus,
  sleep = defaultSleep,
  now = defaultNow,
  timeoutMs = DEFAULT_TIMEOUT_MS,
  intervalMs = DEFAULT_INTERVAL_MS,
}: WaitForUpdateActivationOptions): Promise<ActivationOutcome> {
  assertFinitePositive('timeoutMs', timeoutMs)
  assertFinitePositive('intervalMs', intervalMs)

  const startedAt = now()
  const deadline = startedAt + timeoutMs
  let legacyHealthDisappeared = false

  while (now() < deadline) {
    if (mode === 'binary') {
      let healthy = false
      try {
        const healthResult = await awaitBeforeDeadline(checkHealth, deadline, now)
        if (healthResult.state === 'timeout') {
          return { state: 'timeout' }
        }
        healthy = healthResult.value
      } catch {
        healthy = false
      }

      if (now() >= deadline) {
        return { state: 'timeout' }
      }

      if (!healthy) {
        legacyHealthDisappeared = true
      } else if (legacyHealthDisappeared || now() - startedAt >= LEGACY_HEALTH_GRACE_MS) {
        return { state: 'healthy' }
      }
    } else {
      try {
        const statusResult = await awaitBeforeDeadline(getStatus, deadline, now)
        if (statusResult.state === 'timeout') {
          return { state: 'timeout' }
        }

        if (now() >= deadline) {
          return { state: 'timeout' }
        }

        const status = statusResult.value
        switch (status.state) {
          case 'healthy':
            return { state: 'healthy', status }
          case 'rolled_back':
            return { state: 'rolled_back', status }
          case 'rollback_failed':
            return { state: 'rollback_failed', status }
          case 'failed':
            return { state: 'failed', status }
        }
      } catch {
        // The service or authenticated status endpoint may be unavailable during replacement.
      }
    }

    const remainingMs = deadline - now()
    if (remainingMs <= 0) {
      break
    }
    const sleepResult = await awaitBeforeDeadline(
      () => sleep(Math.min(intervalMs, remainingMs)),
      deadline,
      now,
    )
    if (sleepResult.state === 'timeout' || now() >= deadline) {
      return { state: 'timeout' }
    }
  }

  return { state: 'timeout' }
}
