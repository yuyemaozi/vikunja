<template>
	<div>
		<Message
			v-if="errorMessage"
			variant="danger"
			:class="{'mbe-4': mode !== 'login'}"
		>
			{{ errorMessage }}
		</Message>
		<Message
			v-if="errorMessageFromQuery"
			variant="danger"
			class="mbs-2"
		>
			{{ errorMessageFromQuery }}
		</Message>
		<Message v-if="loading && !needsTotp">
			{{ $t('user.auth.authenticating') }}
		</Message>

		<form
			v-if="mode === 'link'"
			@submit.prevent="linkIdentity"
		>
			<h2 class="title">
				{{ $t('user.settings.linkedAccounts.confirmTitle', {provider: providerName}) }}
			</h2>
			<p class="mbe-4">
				{{ $t('user.settings.linkedAccounts.confirmText', {provider: providerName}) }}
			</p>
			<FormField
				id="linkIdentityPassword"
				v-model="linkPassword"
				v-focus
				:label="$t('user.settings.currentPassword')"
				:placeholder="$t('user.settings.currentPasswordPlaceholder')"
				autocomplete="current-password"
				type="password"
			/>
			<FormField
				v-if="linkNeedsTotp"
				id="linkIdentityPasscode"
				v-model="linkTotpPasscode"
				:label="$t('user.settings.totp.passcode')"
				:placeholder="$t('user.settings.totp.passcodePlaceholder')"
				autocomplete="one-time-code"
				type="text"
				inputmode="numeric"
			/>
			<div class="buttons mbs-4">
				<XButton
					type="submit"
					:loading="linkMutation.isPending.value"
				>
					{{ $t('user.settings.linkedAccounts.confirm') }}
				</XButton>
				<XButton
					variant="tertiary"
					@click="cancelLinking"
				>
					{{ $t('misc.cancel') }}
				</XButton>
			</div>
		</form>

		<XButton
			v-if="mode === 'linkFailed'"
			class="mbs-2"
			:to="{name: 'user.settings.linked-accounts'}"
		>
			{{ $t('user.settings.linkedAccounts.backToSettings') }}
		</XButton>

		<form
			v-if="needsTotp"
			@submit.prevent="submitTotpAndRestart"
		>
			<Message class="mbe-2">
				{{ $t('user.auth.openIdTotpRequired') }}
			</Message>
			<FormField
				id="openIdTotpPasscode"
				ref="totpInput"
				v-model="totpPasscode"
				v-focus
				:label="$t('user.auth.totpTitle')"
				autocomplete="one-time-code"
				:placeholder="$t('user.auth.totpPlaceholder')"
				required
				type="text"
				inputmode="numeric"
			/>
			<XButton
				:loading="loading"
				:disabled="!totpPasscode"
				class="mbs-2"
				@click="submitTotpAndRestart"
			>
				{{ $t('user.auth.openIdTotpSubmit') }}
			</XButton>
		</form>
	</div>
</template>


<script setup lang="ts">
import type {VikunjaErrorModel} from '@/client/generated'
import {ref, computed, onMounted} from 'vue'
import {useRoute, useRouter} from 'vue-router'
import {useI18n} from 'vue-i18n'
import {useQuery} from '@tanstack/vue-query'

import {getErrorText} from '@/message'
import Message from '@/components/misc/Message.vue'
import FormField from '@/components/input/FormField.vue'
import {useRedirectToLastVisited} from '@/composables/useRedirectToLastVisited'
import {
	clearLinkingMarker,
	getRedirectUrlFromCurrentFrontendPath,
	isLinkingIdentity,
	redirectToProvider,
} from '@/helpers/redirectToProvider'
import {useLinkIdentityMutation} from '@/client/queries/identities'
import {totpQuery} from '@/client/queries/totp'
import {isRequestContextAbort} from '@/client/requestContext'

import {useAuthStore} from '@/stores/auth'
import {useConfigStore} from '@/stores/config'
import {ERROR_CODE_TOTP_REQUIRED} from '@/constants/auth'
import type {IProvider} from '@/types/IProvider'

defineOptions({name: 'Auth'})

const {t} = useI18n({useScope: 'global'})

const route = useRoute()
const {redirectIfSaved} = useRedirectToLastVisited()

const authStore = useAuthStore()
const configStore = useConfigStore()

const loading = computed(() => authStore.isLoading)
const errorMessage = ref('')
const errorMessageFromQuery = computed(() => route.query.error)

const needsTotp = ref(false)
const totpPasscode = ref('')

const router = useRouter()
const mode = ref<'login' | 'link' | 'linkFailed'>('login')
const linkPassword = ref('')
const linkTotpPasscode = ref('')
const linkMutation = useLinkIdentityMutation()
const totp = useQuery(computed(() => ({
	...totpQuery(),
	enabled: mode.value === 'link' && configStore.totp_enabled,
})))
const linkNeedsTotp = computed(() => totp.data.value?.enabled === true)
const providerName = computed(() => findProvider(route.params.provider as string)?.name ?? route.params.provider)

// Rejected before the code exchange, so the same code still works on retry.
const RETRYABLE_LINK_ERRORS = new Set([
	1011, // wrong password
	1017, // missing or wrong TOTP passcode
	1039, // TOTP passcode reused
	1041, // too many attempts
])

function pendingTotpKey(provider: string): string {
	return `openid_pending_totp_${provider}`
}

function findProvider(providerKey: string): IProvider | undefined {
	return configStore.auth.openid_connect.providers?.find((p: IProvider) => p.key === providerKey)
}

async function authenticateWithCode() {
	errorMessage.value = ''

	const providerKey = route.params.provider as string

	if (typeof route.query.error !== 'undefined') {
		sessionStorage.removeItem(pendingTotpKey(providerKey))
		errorMessage.value = typeof route.query.message !== 'undefined'
			? route.query.message as string
			: t('user.auth.openIdGeneralError')
		return
	}

	const state = localStorage.getItem('state')
	if (typeof route.query.state === 'undefined' || route.query.state !== state) {
		sessionStorage.removeItem(pendingTotpKey(providerKey))
		errorMessage.value = t('user.auth.openIdStateError')
		return
	}

	if (isLinkingIdentity()) {
		if (!authStore.authUser) {
			clearLinkingMarker()
			errorMessage.value = t('user.settings.linkedAccounts.noSession')
			return
		}
		mode.value = 'link'
		return
	}

	// sessionStorage (not localStorage): per-tab, cleared on tab close.
	const pendingPasscode = sessionStorage.getItem(pendingTotpKey(providerKey)) ?? undefined
	if (pendingPasscode) {
		sessionStorage.removeItem(pendingTotpKey(providerKey))
	}

	try {
		await authStore.openIdAuth({
			provider: providerKey,
			code: route.query.code as string,
			totpPasscode: pendingPasscode,
		})

		redirectIfSaved()
	} catch (e) {
		if ((e as VikunjaErrorModel)?.code === ERROR_CODE_TOTP_REQUIRED) {
			needsTotp.value = true
			return
		}
		errorMessage.value = getErrorText(e)
	}
}

async function submitTotpAndRestart() {
	if (!totpPasscode.value) {
		return
	}

	const providerKey = route.params.provider as string
	const provider = findProvider(providerKey)
	if (!provider) {
		errorMessage.value = t('user.auth.openIdGeneralError')
		return
	}

	sessionStorage.setItem(pendingTotpKey(providerKey), totpPasscode.value)
	// The auth code is single-use; restart the OIDC flow so the next callback reads the stashed passcode.
	redirectToProvider(provider)
}

async function linkIdentity() {
	if (linkMutation.isPending.value) return

	const providerKey = route.params.provider as string
	const provider = findProvider(providerKey)
	if (!provider) {
		errorMessage.value = t('user.auth.openIdGeneralError')
		return
	}

	errorMessage.value = ''
	try {
		await linkMutation.mutateAsync({
			provider: providerKey,
			code: route.query.code as string,
			redirect_url: getRedirectUrlFromCurrentFrontendPath(provider),
			password: linkPassword.value,
			totp_passcode: linkNeedsTotp.value ? linkTotpPasscode.value : undefined,
		})
	} catch (e) {
		if (isRequestContextAbort(e)) return
		errorMessage.value = getErrorText(e)
		const code = (e as VikunjaErrorModel)?.code
		if (code === undefined || !RETRYABLE_LINK_ERRORS.has(code)) {
			clearLinkingMarker()
			mode.value = 'linkFailed'
		}
		return
	}

	clearLinkingMarker()
	await router.push({name: 'user.settings.linked-accounts'})
}

function cancelLinking() {
	clearLinkingMarker()
	router.push({name: 'user.settings.linked-accounts'})
}

onMounted(() => authenticateWithCode())
</script>
