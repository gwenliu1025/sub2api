import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import VersionBadge from '../VersionBadge.vue'

const adminSystemMocks = vi.hoisted(() => ({
  performUpdate: vi.fn(),
  restartService: vi.fn(),
  getRollbackVersions: vi.fn(),
  rollback: vi.fn(),
  getUpdateStatus: vi.fn()
}))

const activationMocks = vi.hoisted(() => ({
  waitForUpdateActivation: vi.fn()
}))

const clipboardMocks = vi.hoisted(() => ({
  copyToClipboard: vi.fn()
}))

const storeMocks = vi.hoisted(() => ({
  authStore: {
    isAdmin: true
  },
  appStore: {
    versionLoading: false,
    currentVersion: '0.1.149',
    latestVersion: '0.1.150',
    hasUpdate: true,
    releaseInfo: {
      name: 'v0.1.150',
      body: '',
      published_at: '2026-07-11T00:00:00Z',
      html_url: '#'
    },
    buildType: 'release',
    updateMode: 'docker_agent' as 'binary' | 'docker_agent',
    fetchVersion: vi.fn(),
    clearVersionCache: vi.fn()
  }
}))

vi.mock('@/api/admin/system', () => ({
  performUpdate: adminSystemMocks.performUpdate,
  restartService: adminSystemMocks.restartService,
  getRollbackVersions: adminSystemMocks.getRollbackVersions,
  rollback: adminSystemMocks.rollback,
  getUpdateStatus: adminSystemMocks.getUpdateStatus
}))

vi.mock('@/utils/updateActivation', () => ({
  waitForUpdateActivation: activationMocks.waitForUpdateActivation
}))

vi.mock('@/stores', () => ({
  useAuthStore: () => storeMocks.authStore,
  useAppStore: () => storeMocks.appStore
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({
    copied: false,
    copyToClipboard: clipboardMocks.copyToClipboard
  })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

type Deferred<T> = {
  promise: Promise<T>
  resolve: (value: T) => void
  reject: (reason?: unknown) => void
}

type AgentState =
  | 'idle'
  | 'preparing'
  | 'prepared'
  | 'activating'
  | 'healthy'
  | 'rolled_back'
  | 'failed'
  | 'rollback_failed'

const mountedWrappers: VueWrapper[] = []
const reload = vi.fn()

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, resolve, reject }
}

function createStatus(state: AgentState, message: string) {
  return {
    state,
    current_image: '',
    target_image: '',
    previous_image: '',
    message,
    updated_at: '2026-07-11T00:00:00Z'
  }
}

function buttonByText(wrapper: VueWrapper, texts: string[]): VueWrapper {
  const button = wrapper
    .findAll('button')
    .find((candidate) => texts.some((text) => candidate.text().includes(text)))

  if (!button) {
    throw new Error(`Unable to find button containing one of: ${texts.join(', ')}`)
  }

  return button
}

async function mountOpenBadge(
  mode: 'binary' | 'docker_agent' = 'docker_agent'
): Promise<VueWrapper> {
  storeMocks.appStore.updateMode = mode
  const wrapper = mount(VersionBadge, {
    global: {
      stubs: {
        Icon: true
      }
    }
  })
  mountedWrappers.push(wrapper)

  await flushPromises()
  await wrapper.get('button').trigger('click')
  await flushPromises()

  return wrapper
}

async function prepareImage(wrapper: VueWrapper): Promise<void> {
  await buttonByText(wrapper, ['version.prepareImage', 'version.updateNow']).trigger('click')
  await flushPromises()
}

async function prepareAndRestart(
  mode: 'binary' | 'docker_agent' = 'docker_agent'
): Promise<VueWrapper> {
  const wrapper = await mountOpenBadge(mode)
  await prepareImage(wrapper)
  await buttonByText(wrapper, ['version.restartNow']).trigger('click')
  await flushPromises()
  return wrapper
}

describe('VersionBadge', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    Object.assign(storeMocks.appStore, {
      versionLoading: false,
      currentVersion: '0.1.149',
      latestVersion: '0.1.150',
      hasUpdate: true,
      releaseInfo: {
        name: 'v0.1.150',
        body: '',
        published_at: '2026-07-11T00:00:00Z',
        html_url: '#'
      },
      buildType: 'release',
      updateMode: 'docker_agent'
    })
    storeMocks.appStore.fetchVersion.mockResolvedValue(null)
    adminSystemMocks.performUpdate.mockResolvedValue({
      message: 'Image prepared',
      need_restart: true
    })
    adminSystemMocks.restartService.mockResolvedValue({
      message: 'Restart requested',
      update_mode: 'docker_agent'
    })
    adminSystemMocks.getRollbackVersions.mockResolvedValue({ versions: [] })
    adminSystemMocks.rollback.mockResolvedValue({
      message: 'Rollback prepared',
      need_restart: true
    })
    activationMocks.waitForUpdateActivation.mockResolvedValue({ state: 'healthy' })

    Object.defineProperty(window, 'location', {
      configurable: true,
      value: { reload }
    })
    vi.stubGlobal('fetch', vi.fn())
  })

  afterEach(() => {
    mountedWrappers.splice(0).forEach((wrapper) => wrapper.unmount())
    vi.unstubAllGlobals()
  })

  it('shows image preparing while the prepare request is pending', async () => {
    const request = deferred<{ message: string; need_restart: boolean }>()
    adminSystemMocks.performUpdate.mockReturnValueOnce(request.promise)
    const wrapper = await mountOpenBadge('docker_agent')

    const prepareButton = buttonByText(wrapper, ['version.prepareImage', 'version.updateNow'])
    await prepareButton.trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('version.preparingImage')
    expect(prepareButton.attributes('disabled')).toBeDefined()

    await prepareButton.trigger('click')
    expect(adminSystemMocks.performUpdate).toHaveBeenCalledTimes(1)

    request.resolve({ message: 'Image prepared', need_restart: true })
    await flushPromises()
  })

  it('shows the prepared version and enables restart after prepare succeeds', async () => {
    const wrapper = await mountOpenBadge('docker_agent')

    await prepareImage(wrapper)

    expect(wrapper.text()).toContain('version.imagePrepared')
    expect(wrapper.text()).toContain('version.activatePrepared')
    expect(wrapper.text()).toContain('v0.1.150')
    expect(buttonByText(wrapper, ['version.restartNow']).attributes('disabled')).toBeUndefined()
  })

  it('shows the sanitized prepare error without truncating it', async () => {
    const prepareError = 'Image verification failed: source label mismatch'
    adminSystemMocks.performUpdate.mockRejectedValueOnce({ message: prepareError })
    const wrapper = await mountOpenBadge('docker_agent')

    await prepareImage(wrapper)

    const errorText = wrapper.findAll('p').find((node) => node.text() === prepareError)
    expect(errorText?.exists()).toBe(true)
    expect(errorText?.classes()).not.toContain('truncate')
    expect(errorText?.classes()).toContain('break-words')
    expect(errorText?.classes()).toContain('whitespace-pre-wrap')
  })

  it('reloads after a healthy activation', async () => {
    activationMocks.waitForUpdateActivation.mockResolvedValueOnce({
      state: 'healthy',
      status: createStatus('healthy', 'new container is healthy')
    })

    await prepareAndRestart('docker_agent')

    expect(activationMocks.waitForUpdateActivation).toHaveBeenCalledWith(
      expect.objectContaining({
        mode: 'docker_agent',
        checkHealth: expect.any(Function),
        getStatus: adminSystemMocks.getUpdateStatus
      })
    )
    expect(storeMocks.appStore.clearVersionCache).toHaveBeenCalled()
    expect(reload).toHaveBeenCalledOnce()
  })

  it('shows automatic rollback instead of reloading', async () => {
    activationMocks.waitForUpdateActivation.mockResolvedValueOnce({
      state: 'rolled_back',
      status: createStatus('rolled_back', 'new image failed health checks')
    })

    const wrapper = await prepareAndRestart('docker_agent')

    expect(reload).not.toHaveBeenCalled()
    expect(wrapper.find('.z-50').exists()).toBe(true)
    expect(wrapper.text()).toContain('version.activationRolledBack')
    expect(wrapper.text()).toContain('new image failed health checks')
  })

  it('shows high severity recovery guidance when rollback fails', async () => {
    activationMocks.waitForUpdateActivation.mockResolvedValueOnce({
      state: 'rollback_failed',
      status: createStatus('rollback_failed', 'previous container failed to start')
    })

    const wrapper = await prepareAndRestart('docker_agent')

    expect(reload).not.toHaveBeenCalled()
    expect(wrapper.find('.z-50').exists()).toBe(true)
    expect(wrapper.text()).toContain('version.activationRollbackFailed')
    expect(wrapper.text()).toContain('version.manualRecoveryRequired')
    expect(wrapper.text()).toContain('previous container failed to start')
  })

  it('keeps the legacy binary update copy', async () => {
    const request = deferred<{ message: string; need_restart: boolean }>()
    adminSystemMocks.performUpdate.mockReturnValueOnce(request.promise)
    const wrapper = await mountOpenBadge('binary')

    expect(wrapper.text()).toContain('version.updateNow')
    await buttonByText(wrapper, ['version.updateNow']).trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('version.updating')

    request.resolve({ message: 'Binary updated', need_restart: true })
    await flushPromises()

    expect(wrapper.text()).toContain('version.updateComplete')
    expect(wrapper.text()).toContain('version.restartRequired')
    expect(wrapper.text()).toContain('version.restartNow')
  })

  it.each([
    {
      label: 'failed',
      outcome: {
        state: 'failed',
        status: createStatus('failed', 'activation command exited with status 1')
      },
      expectedTitle: 'version.activationFailed',
      expectedMessage: 'activation command exited with status 1'
    },
    {
      label: 'timeout',
      outcome: { state: 'timeout' },
      expectedTitle: 'version.activationTimedOut',
      expectedMessage: ''
    }
  ])('keeps the page open when activation is $label', async ({ outcome, expectedTitle, expectedMessage }) => {
    activationMocks.waitForUpdateActivation.mockResolvedValueOnce(outcome)

    const wrapper = await prepareAndRestart('docker_agent')

    expect(reload).not.toHaveBeenCalled()
    expect(wrapper.find('.z-50').exists()).toBe(true)
    expect(wrapper.text()).toContain(expectedTitle)
    if (expectedMessage) {
      expect(wrapper.text()).toContain(expectedMessage)
    }
  })

  it('continues legacy polling when the binary restart request disconnects', async () => {
    adminSystemMocks.restartService.mockRejectedValueOnce(new Error('Network Error'))
    const fetchMock = vi.mocked(fetch)
    fetchMock.mockResolvedValueOnce({ ok: true } as Response)
    activationMocks.waitForUpdateActivation.mockImplementationOnce(
      async ({ mode, checkHealth }: { mode: string; checkHealth: () => Promise<boolean> }) => {
        expect(mode).toBe('binary')
        expect(await checkHealth()).toBe(true)
        return { state: 'healthy' }
      }
    )

    await prepareAndRestart('binary')

    expect(fetchMock).toHaveBeenCalledWith('/health', {
      method: 'GET',
      cache: 'no-cache'
    })
    expect(reload).toHaveBeenCalledOnce()
  })

  it('shows a structured docker business error without polling', async () => {
    adminSystemMocks.restartService.mockRejectedValueOnce({
      status: 409,
      code: 'UPDATE_BUSY',
      message: 'Another update is in progress'
    })

    const wrapper = await prepareAndRestart('docker_agent')

    expect(activationMocks.waitForUpdateActivation).not.toHaveBeenCalled()
    expect(reload).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('version.activationFailed')
    expect(wrapper.text()).toContain('Another update is in progress')
  })

  it('continues docker polling when restart reports a status zero disconnect', async () => {
    adminSystemMocks.restartService.mockRejectedValueOnce({
      status: 0,
      message: 'Network error. Please check your connection.'
    })

    await prepareAndRestart('docker_agent')

    expect(activationMocks.waitForUpdateActivation).toHaveBeenCalledWith(
      expect.objectContaining({
        mode: 'docker_agent'
      })
    )
    expect(reload).toHaveBeenCalledOnce()
  })

  it('uses the custom repository and GHCR image in manual rollback commands', async () => {
    storeMocks.appStore.hasUpdate = false
    adminSystemMocks.getRollbackVersions.mockResolvedValueOnce({
      versions: [
        {
          version: '0.1.148',
          published_at: '2026-07-10T00:00:00Z',
          html_url: '#'
        }
      ]
    })
    const wrapper = await mountOpenBadge('docker_agent')

    await buttonByText(wrapper, ['version.rollback']).trigger('click')
    await flushPromises()
    await buttonByText(wrapper, ['v0.1.148']).trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain(
      'https://raw.githubusercontent.com/gwenliu1025/sub2api/v0.1.148/deploy/install.sh'
    )

    await buttonByText(wrapper, ['version.deployDocker']).trigger('click')
    expect(wrapper.text()).toContain('image: ghcr.io/gwenliu1025/sub2api:0.1.148')
  })
})
