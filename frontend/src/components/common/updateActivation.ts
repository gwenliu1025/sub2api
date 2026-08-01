import type { UpdateAgentStatus } from '@/api/admin/system'

type ActivationOutcome = 'healthy' | 'failed' | 'timeout'

export interface ActivationResult {
  outcome: ActivationOutcome
  status?: UpdateAgentStatus
}

interface ActivationPollingOptions {
  maxAttempts?: number
  delay?: () => Promise<void>
}

const failedStates = new Set<UpdateAgentStatus['state']>([
  'rolled_back',
  'failed',
  'rollback_failed'
])

export async function waitForUpdateActivation(
  getStatus: () => Promise<UpdateAgentStatus>,
  options: ActivationPollingOptions = {}
): Promise<ActivationResult> {
  const maxAttempts = options.maxAttempts ?? 90
  const delay = options.delay ?? (() => new Promise((resolve) => setTimeout(resolve, 2000)))
  let latestStatus: UpdateAgentStatus | undefined

  for (let attempt = 0; attempt < maxAttempts; attempt++) {
    try {
      latestStatus = await getStatus()
      if (latestStatus.state === 'healthy') {
        return { outcome: 'healthy', status: latestStatus }
      }
      if (failedStates.has(latestStatus.state)) {
        return { outcome: 'failed', status: latestStatus }
      }
    } catch {
      // 激活期间应用容器会短暂不可达，继续等待代理状态恢复。
    }

    if (attempt < maxAttempts - 1) {
      await delay()
    }
  }

  return { outcome: 'timeout', status: latestStatus }
}
