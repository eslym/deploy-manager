<script lang="ts">
	import { goto } from '$app/navigation';
	import { BrowserSDKError, sdk } from '../../lib/sdk/index.js';

	let status = $state('Enter your password to sign in.');
	let busy = $state(false);

	class LoginError extends Error {}

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		if (busy) return;

		const form = event.currentTarget as HTMLFormElement;
		const passwordField = form.elements.namedItem('password') as HTMLInputElement;
		let password = passwordField.value;
		passwordField.value = '';
		busy = true;
		status = 'Signing in…';

		try {
			// Load OPAQUE only when the user submits the login form.
			const opaque = await import('@serenity-kit/opaque');
			await opaque.ready;
			if (typeof opaque.client?.startLogin !== 'function') {
				throw new LoginError(
					'Secure sign-in could not start. Check that JavaScript and WebAssembly are enabled, then reload this page.'
				);
			}

			const start = opaque.client.startLogin({ password });
			const challenge = (await sdk.auth.loginStart({ value: start.startLoginRequest })).data;
			if (!challenge.transaction)
				throw new LoginError('Sign-in failed. Check your password and try again.');
			if (typeof opaque.client?.finishLogin !== 'function') {
				throw new LoginError('Secure sign-in could not finish. Reload this page and try again.');
			}
			const keyStretching = {
				'argon2id-custom': { iterations: 1, memory: 65536, parallelism: 4 }
			};
			const finish = opaque.client.finishLogin({
				password,
				loginResponse: challenge.value,
				clientLoginState: start.clientLoginState,
				keyStretching
			});
			if (!finish?.finishLoginRequest) {
				throw new LoginError('Sign-in failed. Check your password and try again.');
			}
			password = '';
			await sdk.auth.loginFinish({
				transaction: challenge.transaction,
				value: finish.finishLoginRequest
			});
			await goto('/dashboard', { replaceState: true });
		} catch (error) {
			if (error instanceof BrowserSDKError) {
				status =
					error.outcome.kind === 'server' && error.outcome.code === 'auth.login_failed'
						? 'Sign-in failed. Check your password and try again.'
						: 'Sign-in could not be completed. Check your connection and try again.';
			} else {
				status =
					error instanceof LoginError
						? error.message
						: 'Secure sign-in could not be initialized or completed. Check that JavaScript and WebAssembly are enabled, then reload and try again.';
			}
		} finally {
			password = '';
			passwordField.value = '';
			busy = false;
		}
	}
</script>

<svelte:head>
	<title>Sign in · Deploy Manager</title>
	<meta name="referrer" content="no-referrer" />
</svelte:head>

<main
	class="mx-auto my-[clamp(2rem,12vh,7rem)] w-[min(100%-2rem,28rem)] rounded-2xl border border-[#e1e6df] bg-white p-[clamp(1.5rem,6vw,2.5rem)] shadow-[0_1.5rem_4rem_rgb(25_44_28_/_8%)]"
>
	<p class="mb-3 text-xs font-bold tracking-[0.12em] text-[#526957] uppercase">Deploy Manager</p>
	<h1 class="m-0 text-[clamp(1.8rem,7vw,2.35rem)] tracking-[-0.04em]">Sign in</h1>
	<p class="leading-[1.55] text-[#58645b]">Sign in to manage deployments.</p>
	<p class="my-6 min-h-6 leading-6" role="status" aria-live="polite">{status}</p>
	<form class="grid gap-[0.7rem]" onsubmit={submit}>
		<label class="font-semibold" for="password">Password</label>
		<input
			class="w-full rounded-lg border border-[#aeb9ae] px-3.5 py-3 font-[inherit] focus-visible:outline-3 focus-visible:outline-offset-2 focus-visible:outline-[#82b392]"
			id="password"
			name="password"
			type="password"
			autocomplete="current-password"
			required
			disabled={busy}
		/>
		<button
			class="mt-2 min-h-12 cursor-pointer rounded-lg border-0 bg-[#214c30] font-[inherit] font-semibold text-white focus-visible:outline-3 focus-visible:outline-offset-2 focus-visible:outline-[#82b392] disabled:cursor-wait disabled:opacity-70"
			type="submit"
			disabled={busy}>{busy ? 'Signing in…' : 'Sign in'}</button
		>
	</form>
</main>
