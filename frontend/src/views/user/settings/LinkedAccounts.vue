<script setup lang="ts">
import {computed, ref} from 'vue'
import {useI18n} from 'vue-i18n'
import {useQuery} from '@tanstack/vue-query'

import {useTitle} from '@/composables/useTitle'
import {useConfigStore} from '@/stores/config'
import {identitiesQuery, useUnlinkIdentityMutation} from '@/client/queries/identities'
import {totpQuery} from '@/client/queries/totp'
import type {LinkedIdentity} from '@/client/generated'
import {formatDisplayDate} from '@/helpers/time/formatDate'
import {redirectToProvider} from '@/helpers/redirectToProvider'
import ErrorMessage from '@/components/misc/Error.vue'
import FormField from '@/components/input/FormField.vue'
import type {IProvider} from '@/types/IProvider'

defineOptions({name: 'UserSettingsLinkedAccounts'})

const {t} = useI18n({useScope: 'global'})
useTitle(() => `${t('user.settings.linkedAccounts.title')} - ${t('user.settings.title')}`)

const configStore = useConfigStore()
const identityQuery = useQuery(identitiesQuery())
const identities = computed(() => identityQuery.data.value ?? [])
const totp = useQuery(computed(() => ({
	...totpQuery(),
	enabled: configStore.totp_enabled,
})))
const totpEnabled = computed(() => totp.data.value?.enabled === true)

const unlinkedProviders = computed(() => {
	const linked = new Set(identities.value.map(i => i.provider_key))
	return configStore.auth.openid_connect.providers.filter((p: IProvider) => !linked.has(p.key))
})

function identityName(identity: LinkedIdentity): string {
	return identity.provider_name || identity.issuer || ''
}

const unlinkMutation = useUnlinkIdentityMutation()
const identityToUnlink = ref<LinkedIdentity | null>(null)
const unlinkTitle = computed(() => identityToUnlink.value
	? t('user.settings.linkedAccounts.unlinkHeader', {provider: identityName(identityToUnlink.value)})
	: '')
const password = ref('')
const totpPasscode = ref('')

function openUnlink(identity: LinkedIdentity) {
	identityToUnlink.value = identity
	password.value = ''
	totpPasscode.value = ''
}

function closeUnlink() {
	identityToUnlink.value = null
	password.value = ''
	totpPasscode.value = ''
}

async function unlink() {
	const identity = identityToUnlink.value
	if (unlinkMutation.isPending.value || !identity?.id) return
	try {
		await unlinkMutation.mutateAsync({
			id: identity.id,
			password: password.value,
			totp_passcode: totpEnabled.value ? totpPasscode.value : undefined,
		})
	} catch { return }
	if (identityToUnlink.value === identity) closeUnlink()
}
</script>

<template>
	<Card
		:title="$t('user.settings.linkedAccounts.title')"
		:loading="identityQuery.isPending.value"
	>
		<p class="mbe-4">
			{{ $t('user.settings.linkedAccounts.description') }}
		</p>

		<ErrorMessage v-if="identityQuery.isError.value" />

		<div
			v-if="identities.length > 0"
			class="has-horizontal-overflow"
		>
			<table class="table">
				<thead>
					<tr>
						<th>{{ $t('user.settings.linkedAccounts.provider') }}</th>
						<th>{{ $t('user.settings.linkedAccounts.linkedAt') }}</th>
						<th class="has-text-end">
							{{ $t('misc.actions') }}
						</th>
					</tr>
				</thead>
				<tbody>
					<tr
						v-for="identity in identities"
						:key="identity.id"
					>
						<td>{{ identityName(identity) }}</td>
						<td>{{ formatDisplayDate(identity.created) }}</td>
						<td class="has-text-end">
							<XButton
								variant="secondary"
								@click="openUnlink(identity)"
							>
								{{ $t('user.settings.linkedAccounts.unlink') }}
							</XButton>
						</td>
					</tr>
				</tbody>
			</table>
		</div>

		<p
			v-else-if="identityQuery.isSuccess.value"
			class="mbe-4"
		>
			{{ $t('user.settings.linkedAccounts.none') }}
		</p>

		<div
			v-if="identityQuery.isSuccess.value && unlinkedProviders.length > 0"
			class="link-buttons"
		>
			<XButton
				v-for="provider in unlinkedProviders"
				:key="provider.key"
				icon="link"
				@click="redirectToProvider(provider, {linking: true})"
			>
				{{ $t('user.settings.linkedAccounts.link', {provider: provider.name}) }}
			</XButton>
		</div>

		<Modal
			v-if="identityToUnlink"
			variant="hint-modal"
			:aria-label="unlinkTitle"
			@close="closeUnlink()"
		>
			<Card
				class="has-no-shadow"
				:title="unlinkTitle"
			>
				<p class="mbe-4">
					{{ $t('user.settings.linkedAccounts.unlinkText') }}
				</p>
				<FormField
					id="unlinkIdentityPassword"
					v-model="password"
					v-focus
					:label="$t('user.settings.currentPassword')"
					:placeholder="$t('user.settings.currentPasswordPlaceholder')"
					autocomplete="current-password"
					type="password"
					@keyup.enter="unlink()"
				/>
				<FormField
					v-if="totpEnabled"
					id="unlinkIdentityPasscode"
					v-model="totpPasscode"
					:label="$t('user.settings.totp.passcode')"
					:placeholder="$t('user.settings.totp.passcodePlaceholder')"
					autocomplete="one-time-code"
					type="text"
					inputmode="numeric"
					@keyup.enter="unlink()"
				/>

				<template #footer>
					<XButton
						variant="tertiary"
						@click="closeUnlink()"
					>
						{{ $t('misc.cancel') }}
					</XButton>
					<XButton
						danger
						:loading="unlinkMutation.isPending.value"
						@click="unlink()"
					>
						{{ $t('user.settings.linkedAccounts.unlink') }}
					</XButton>
				</template>
			</Card>
		</Modal>
	</Card>
</template>

<style lang="scss" scoped>
.link-buttons {
	display: flex;
	flex-wrap: wrap;
	gap: .5rem;
}
</style>
