<script lang="ts">
	import { goto } from '$app/navigation';
	import { BrowserSDKError, sdk } from '../../../lib/sdk/index.js';

	let status = $state('Confirm your current password, then choose a new one.');
	let busy = $state(false);
	let complete = $state(false);

	class PasswordChangeError extends Error {}

	async function send<T>(request: () => Promise<{ data: T }>): Promise<T> {
		try {
			return (await request()).data;
		} catch (error) {
			if (
				error instanceof BrowserSDKError &&
				error.outcome.kind === 'server' &&
				error.outcome.code === 'auth.session_required'
			)
				throw new PasswordChangeError(
					'Your session is no longer valid. Sign in again, then retry.'
				);
			if (
				error instanceof BrowserSDKError &&
				error.outcome.kind === 'server' &&
				error.outcome.code === 'auth.login_failed'
			)
				throw new PasswordChangeError(
					'Your current password was not accepted. Check it and try again.'
				);
			if (
				error instanceof BrowserSDKError &&
				error.outcome.kind === 'server' &&
				error.outcome.code === 'auth.recent_login_required'
			)
				throw new PasswordChangeError('For security, sign in again before changing your password.');
			throw new PasswordChangeError(
				'The secure password change could not be completed. Check your connection and try again.'
			);
		}
	}

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		if (busy) return;
		const form = event.currentTarget as HTMLFormElement;
		const currentField = form.elements.namedItem('current-password') as HTMLInputElement;
		const newField = form.elements.namedItem('new-password') as HTMLInputElement;
		let currentPassword = currentField.value;
		let newPassword = newField.value;
		currentField.value = '';
		newField.value = '';
		if (newPassword.length < 12) {
			currentPassword = '';
			newPassword = '';
			status = 'Use a new password with at least 12 characters.';
			return;
		}
		busy = true;
		status = 'Verifying your current password securely…';
		try {
			// Keep OPAQUE/WASM out of shared chunks until the user submits.
			const opaque = await import('@serenity-kit/opaque');
			await opaque.ready;
			if (
				typeof opaque.client?.startLogin !== 'function' ||
				typeof opaque.client?.startRegistration !== 'function'
			) {
				throw new PasswordChangeError(
					'Secure password change could not start. Enable JavaScript and WebAssembly, then reload.'
				);
			}
			const keyStretching = {
				'argon2id-custom': { iterations: 1, memory: 65536, parallelism: 4 }
			};
			let loginStart: ReturnType<typeof opaque.client.startLogin> | null = opaque.client.startLogin(
				{ password: currentPassword }
			);
			let loginState = loginStart.clientLoginState;
			const challenge = await send<{ value: string; transaction?: string }>(() =>
				sdk.auth.loginStart({ value: loginStart!.startLoginRequest })
			);
			loginStart = null;
			const loginTransaction = challenge?.transaction;
			if (!loginTransaction || !challenge?.value) {
				throw new PasswordChangeError(
					'Could not verify your current password. Check it and try again.'
				);
			}
			let loginFinish: ReturnType<typeof opaque.client.finishLogin> | null =
				opaque.client.finishLogin({
					password: currentPassword,
					loginResponse: challenge.value,
					clientLoginState: loginState,
					keyStretching
				});
			loginState = '';
			currentPassword = '';
			if (!loginFinish?.finishLoginRequest) {
				throw new PasswordChangeError('Current password was not accepted. Check it and try again.');
			}
			const loginRequest = loginFinish.finishLoginRequest;
			loginFinish = null;
			await send(() =>
				sdk.auth.loginFinish({ transaction: loginTransaction, value: loginRequest })
			);
			status = 'Current password verified. Creating a fresh secure registration…';
			let registrationStart: ReturnType<typeof opaque.client.startRegistration> | null =
				opaque.client.startRegistration({ password: newPassword });
			const registrationResponse = await send<{ value: string; transaction?: string }>(() =>
				sdk.auth.passwordChangeStart({ value: registrationStart!.registrationRequest })
			);
			if (!registrationResponse?.value) {
				throw new PasswordChangeError(
					'Could not start the new password registration. Retry the change.'
				);
			}
			let registrationFinish: ReturnType<typeof opaque.client.finishRegistration> | null =
				opaque.client.finishRegistration({
					password: newPassword,
					registrationResponse: registrationResponse.value,
					clientRegistrationState: registrationStart.clientRegistrationState,
					keyStretching
				});
			newPassword = '';
			registrationStart = null;
			if (!registrationFinish?.registrationRecord) {
				throw new PasswordChangeError(
					'Could not create the new password record. Retry the change.'
				);
			}
			const record = registrationFinish.registrationRecord;
			registrationFinish = null;
			await send(() => sdk.auth.passwordChangeFinish({ value: record }));
			complete = true;
			status =
				'Password changed. Your sessions have been signed out; sign in with your new password.';
		} catch (error) {
			status =
				error instanceof PasswordChangeError
					? error.message
					: 'Secure password change could not be initialized or completed. Enable JavaScript and WebAssembly, then reload and try again.';
		} finally {
			currentPassword = '';
			newPassword = '';
			currentField.value = '';
			newField.value = '';
			busy = false;
		}
	}
</script>

<svelte:head>
	<title>Change password · Deploy Manager</title>
	<meta name="referrer" content="no-referrer" />
</svelte:head>

<main
	class="mx-auto my-10 w-[min(100%-2rem,40rem)] rounded-2xl border border-[#e1e6df] bg-white p-[clamp(1.5rem,6vw,2.5rem)] shadow-[0_1.5rem_4rem_rgb(25_44_28_/_8%)]"
>
	<a class="text-sm text-[#214c30] underline underline-offset-4" href="/dashboard">← Dashboard</a>
	<p class="mt-6 mb-3 text-xs font-bold tracking-[0.12em] text-[#526957] uppercase">
		Account security
	</p>
	<h1 class="m-0 text-[clamp(1.8rem,7vw,2.35rem)] tracking-[-0.04em]">Change password</h1>
	<p class="leading-[1.55] text-[#58645b]">
		Your passwords stay in this browser. You’ll be signed out after the change.
	</p>
	<p class="my-6 min-h-6 leading-6" role="status" aria-live="polite">{status}</p>
	{#if !complete}
		<form class="grid gap-[0.7rem]" onsubmit={submit}>
			<label class="font-semibold" for="current-password">Current password</label>
			<input
				class="w-full rounded-lg border border-[#aeb9ae] px-3.5 py-3 font-[inherit] focus-visible:outline-3 focus-visible:outline-offset-2 focus-visible:outline-[#82b392]"
				id="current-password"
				name="current-password"
				type="password"
				autocomplete="current-password"
				required
				disabled={busy}
			/>
			<label class="mt-2 font-semibold" for="new-password">New password</label>
			<input
				class="w-full rounded-lg border border-[#aeb9ae] px-3.5 py-3 font-[inherit] focus-visible:outline-3 focus-visible:outline-offset-2 focus-visible:outline-[#82b392]"
				id="new-password"
				name="new-password"
				type="password"
				autocomplete="new-password"
				minlength="12"
				required
				disabled={busy}
			/>
			<p class="mt-0 mb-2 text-sm leading-[1.55] text-[#58645b]">Use at least 12 characters.</p>
			<button
				class="min-h-12 cursor-pointer rounded-lg border-0 bg-[#214c30] font-[inherit] font-semibold text-white focus-visible:outline-3 focus-visible:outline-offset-2 focus-visible:outline-[#82b392] disabled:cursor-wait disabled:opacity-70"
				type="submit"
				disabled={busy}
			>
				{busy ? 'Changing password…' : 'Change password'}
			</button>
		</form>
	{:else}
		<button
			class="min-h-12 cursor-pointer rounded-lg border-0 bg-[#214c30] px-5 font-[inherit] font-semibold text-white focus-visible:outline-3 focus-visible:outline-offset-2 focus-visible:outline-[#82b392]"
			onclick={() => goto('/login', { replaceState: true })}
		>
			Continue to sign in
		</button>
	{/if}
</main>
