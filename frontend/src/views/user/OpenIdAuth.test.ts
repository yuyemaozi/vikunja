import {QueryClient, VueQueryPlugin} from '@tanstack/vue-query'
import {beforeEach, describe, it, expect, vi} from 'vitest'
import {mount, flushPromises} from '@vue/test-utils'
import {createI18n} from 'vue-i18n'
import OpenIdAuth from './OpenIdAuth.vue'
import en from '@/i18n/lang/en.json'

const {sdk, mockRoute, router, authStore, redirectIfSaved} = vi.hoisted(() => ({
	sdk: {
		totpGet: vi.fn(),
		userIdentitiesLink: vi.fn(),
	},
	mockRoute: {
		params: {provider: 'dex'} as Record<string, string>,
		query: {} as Record<string, string>,
	},
	router: {push: vi.fn()},
	authStore: {
		authUser: false,
		isLoading: false,
		openIdAuth: vi.fn(),
	},
	redirectIfSaved: vi.fn(),
}))

vi.mock('@/client/generated', () => sdk)
vi.mock('@/message', async importOriginal => ({
	...await importOriginal<typeof import('@/message')>(),
	success: vi.fn(),
	error: vi.fn(),
}))
vi.mock('vue-router', () => ({
	useRoute: () => mockRoute,
	useRouter: () => router,
}))
vi.mock('@/stores/auth', () => ({useAuthStore: () => authStore}))
vi.mock('@/stores/config', () => ({
	useConfigStore: () => ({
		totp_enabled: true,
		auth: {
			openid_connect: {
				enabled: true,
				providers: [{
					key: 'dex',
					name: 'Dex',
					auth_url: 'http://dex:5556/auth',
					client_id: 'vikunja',
					scope: 'openid',
					logout_url: '',
				}],
			},
		},
	}),
}))
vi.mock('@/composables/useRedirectToLastVisited', () => ({
	useRedirectToLastVisited: () => ({redirectIfSaved}),
}))

const i18n = createI18n({legacy: false, locale: 'en', messages: {en}})

function mountCallback() {
	return mount(OpenIdAuth, {
		global: {
			plugins: [i18n, [VueQueryPlugin, {queryClient: new QueryClient()}]],
			stubs: {XButton: true},
			directives: {focus: {}},
		},
	})
}

beforeEach(() => {
	vi.clearAllMocks()
	localStorage.clear()
	sessionStorage.clear()
	localStorage.setItem('state', 'expected-state')
	mockRoute.query = {code: 'auth-code', state: 'expected-state'}
	authStore.authUser = false
	authStore.openIdAuth.mockResolvedValue(undefined)
	sdk.totpGet.mockResolvedValue({data: {enabled: false}})
})

describe('OpenIdAuth login', () => {
	it('refuses a callback whose state does not match', async () => {
		mockRoute.query = {code: 'auth-code', state: 'forged'}
		const wrapper = mountCallback()
		await flushPromises()

		expect(authStore.openIdAuth).not.toHaveBeenCalled()
		expect(wrapper.text()).toContain('State does not match')
	})

	it('logs in with the code', async () => {
		mountCallback()
		await flushPromises()

		expect(authStore.openIdAuth).toHaveBeenCalledWith({
			provider: 'dex',
			code: 'auth-code',
			totpPasscode: undefined,
		})
		expect(redirectIfSaved).toHaveBeenCalled()
	})

	it('asks for a TOTP passcode when the login requires one', async () => {
		authStore.openIdAuth.mockRejectedValue({code: 1017})
		const wrapper = mountCallback()
		await flushPromises()

		expect(wrapper.find('#openIdTotpPasscode').exists()).toBe(true)
		expect(redirectIfSaved).not.toHaveBeenCalled()
	})
})

describe('OpenIdAuth linking', () => {
	beforeEach(() => {
		localStorage.setItem('openidLinking', 'true')
		authStore.authUser = true
		sdk.userIdentitiesLink.mockResolvedValue({data: {id: 1, provider_key: 'dex'}})
	})

	async function submitLink(wrapper: ReturnType<typeof mountCallback>, password: string) {
		await wrapper.find('#linkIdentityPassword').setValue(password)
		await wrapper.find('form').trigger('submit')
		await flushPromises()
	}

	it('asks for the password instead of logging in', async () => {
		const wrapper = mountCallback()
		await flushPromises()

		expect(authStore.openIdAuth).not.toHaveBeenCalled()
		expect(wrapper.text()).toContain('Link Dex to your account')
		expect(wrapper.find('#linkIdentityPasscode').exists()).toBe(false)
	})

	it('links the identity and returns to the settings', async () => {
		const wrapper = mountCallback()
		await flushPromises()
		await submitLink(wrapper, 'secret')

		expect(sdk.userIdentitiesLink).toHaveBeenCalledWith({
			body: {
				provider: 'dex',
				code: 'auth-code',
				redirect_url: 'http://localhost:3000/auth/openid/dex',
				password: 'secret',
				totp_passcode: undefined,
			},
		})
		expect(localStorage.getItem('openidLinking')).toBeNull()
		expect(router.push).toHaveBeenCalledWith({name: 'user.settings.linked-accounts'})
	})

	it('sends the TOTP passcode when TOTP is enabled', async () => {
		sdk.totpGet.mockResolvedValue({data: {enabled: true}})
		const wrapper = mountCallback()
		await flushPromises()
		await wrapper.find('#linkIdentityPasscode').setValue('123456')
		await submitLink(wrapper, 'secret')

		expect(sdk.userIdentitiesLink).toHaveBeenCalledWith({
			body: expect.objectContaining({
				password: 'secret',
				totp_passcode: '123456',
			}),
		})
	})

	it('keeps the form for a retry after a wrong password', async () => {
		sdk.userIdentitiesLink.mockRejectedValueOnce({status: 403, code: 1011})
		const wrapper = mountCallback()
		await flushPromises()
		await submitLink(wrapper, 'wrong')

		expect(wrapper.text()).toContain('Wrong username or password.')
		expect(localStorage.getItem('openidLinking')).toBe('true')
		expect(router.push).not.toHaveBeenCalled()

		await submitLink(wrapper, 'secret')

		expect(sdk.userIdentitiesLink).toHaveBeenCalledTimes(2)
		expect(sdk.userIdentitiesLink).toHaveBeenLastCalledWith({
			body: expect.objectContaining({password: 'secret', code: 'auth-code'}),
		})
		expect(router.push).toHaveBeenCalledWith({name: 'user.settings.linked-accounts'})
	})

	it('drops the form once the code was used up', async () => {
		sdk.userIdentitiesLink.mockRejectedValue({status: 409, code: 15002})
		const wrapper = mountCallback()
		await flushPromises()
		await submitLink(wrapper, 'secret')

		expect(wrapper.text()).toContain('already linked to another Vikunja account')
		expect(wrapper.find('form').exists()).toBe(false)
		expect(localStorage.getItem('openidLinking')).toBeNull()
	})

	it('refuses to link or log in without a session', async () => {
		authStore.authUser = false
		const wrapper = mountCallback()
		await flushPromises()

		expect(authStore.openIdAuth).not.toHaveBeenCalled()
		expect(sdk.userIdentitiesLink).not.toHaveBeenCalled()
		expect(wrapper.find('form').exists()).toBe(false)
		expect(wrapper.text()).toContain('You need to be logged in to link an account.')
		expect(localStorage.getItem('openidLinking')).toBeNull()
	})

	it('checks the state before entering link mode', async () => {
		mockRoute.query = {code: 'auth-code', state: 'forged'}
		const wrapper = mountCallback()
		await flushPromises()

		expect(wrapper.find('form').exists()).toBe(false)
		expect(wrapper.text()).toContain('State does not match')
	})
})
