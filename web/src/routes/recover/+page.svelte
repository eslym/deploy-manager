<script lang="ts">
	import { clearSetupGrant, getSetupGrant, setupGrant } from '../../lib/setup-grant.svelte.js';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();
	let grantAvailable = $derived(data.hasGrant && setupGrant.ready);
	let status = $state('');
	let statusMessage = $derived(
		status ||
			(!data.hasGrant
				? 'This recovery link is incomplete. Ask an administrator to issue a new one.'
				: grantAvailable
					? 'Choose a new password for the administrator account.'
					: 'Securing your recovery link…')
	);
	let busy = $state(false);
	let complete = $state(false);

	class RegistrationError extends Error {}

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		const grant = getSetupGrant();
		if (!grant || busy) return;

		const form = event.currentTarget as HTMLFormElement;
		const passwordField = form.elements.namedItem('password') as HTMLInputElement;
		let password = passwordField.value;
		passwordField.value = '';
		if (password.length < 12) {
			password = '';
			status = 'Use a password with at least 12 characters.';
			return;
		}

		busy = true;
		status = 'Replacing the administrator password securely…';
		try {
			// Keep the OPAQUE module out of the shared SPA bundle and load it only on explicit submit.
			const opaque = await import('@serenity-kit/opaque');
			await opaque.ready;
			if (typeof opaque.client?.startRegistration !== 'function') {
				throw new RegistrationError(
					'Secure recovery could not start. Check that JavaScript and WebAssembly are enabled, then reload this page.'
				);
			}

			const profile = { 'argon2id-custom': { iterations: 1, memory: 65536, parallelism: 4 } };
			const start = opaque.client.startRegistration({ password });
			const response = await send('/opaque/register/start', start.registrationRequest, grant);
			if (
				!response ||
				typeof response.value !== 'string' ||
				typeof response.transaction !== 'string' ||
				response.transaction.length === 0
			)
				throw new RegistrationError(
					'Recovery could not be completed. Ask an administrator for a new recovery link.'
				);
			const finish = opaque.client.finishRegistration({
				password,
				registrationResponse: response.value,
				clientRegistrationState: start.clientRegistrationState,
				keyStretching: profile
			});
			password = '';
			await send('/opaque/register/finish', finish.registrationRecord, response.transaction);
			clearSetupGrant();
			complete = true;
			status = 'Administrator password replaced. You can close this page.';
		} catch (error) {
			status =
				error instanceof RegistrationError
					? error.message
					: 'Secure recovery could not be initialized or completed. Check that JavaScript and WebAssembly are enabled, then reload and try again.';
		} finally {
			password = '';
			passwordField.value = '';
			busy = false;
		}
	}

	async function send(
		path: string,
		value: string,
		credential: string
	): Promise<{ value: string; transaction?: string } | null> {
		const response = await fetch(path, {
			method: 'POST',
			credentials: 'same-origin',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify(
				path === '/opaque/register/start'
					? { grant: credential, value }
					: { transaction: credential, value }
			)
		});
		if (!response.ok) {
			if (response.status === 410) {
				throw new RegistrationError(
					'This recovery link has expired or was already used. Ask an administrator for a new one.'
				);
			}
			throw new RegistrationError(
				'Recovery could not be completed. Check your connection and try again.'
			);
		}
		return response.status === 204 ? null : response.json();
	}
</script>

<svelte:head>
	<title>Recover administrator · Deploy Manager</title>
	<meta name="referrer" content="no-referrer" />
</svelte:head>

<main
	class="mx-auto my-[clamp(2rem,12vh,7rem)] w-[min(100%-2rem,28rem)] rounded-2xl border border-[#e1e6df] bg-white p-[clamp(1.5rem,6vw,2.5rem)] shadow-[0_1.5rem_4rem_rgb(25_44_28_/_8%)]"
>
	<p class="mb-3 text-xs font-bold tracking-[0.12em] text-[#526957] uppercase">Deploy Manager</p>
	<h1 class="m-0 text-[clamp(1.8rem,7vw,2.35rem)] tracking-[-0.04em]">Recover administrator</h1>
	<p class="leading-[1.55] text-[#58645b]">
		Replace the administrator password to regain access to this deployment manager.
	</p>
	<p class="my-6 min-h-6 leading-6" role="status" aria-live="polite">{statusMessage}</p>
	{#if grantAvailable && !complete}
		<form class="grid gap-[0.7rem]" onsubmit={submit}>
			<label class="font-semibold" for="password">Replace password</label>
			<input
				class="w-full rounded-lg border border-[#aeb9ae] px-3.5 py-3 font-[inherit] focus-visible:outline-3 focus-visible:outline-offset-2 focus-visible:outline-[#82b392]"
				id="password"
				name="password"
				type="password"
				autocomplete="new-password"
				minlength="12"
				required
				disabled={busy}
			/>
			<p class="mt-0 mb-2 text-sm leading-[1.55] text-[#58645b]">
				Use at least 12 characters. Your password stays in this browser.
			</p>
			<button
				class="min-h-12 cursor-pointer rounded-lg border-0 bg-[#214c30] font-[inherit] font-semibold text-white focus-visible:outline-3 focus-visible:outline-offset-2 focus-visible:outline-[#82b392] disabled:cursor-wait disabled:opacity-70"
				type="submit"
				disabled={busy}>{busy ? 'Replacing password…' : 'Replace password'}</button
			>
		</form>
	{/if}
	<a class="mt-6 inline-block text-sm text-[#214c30] underline underline-offset-4" href="/"
		>Return to start</a
	>
</main>
