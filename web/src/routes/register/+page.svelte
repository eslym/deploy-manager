<script lang="ts">
	import { afterNavigate, goto } from '$app/navigation';
	import { setupGrant } from '../../lib/setup-grant.svelte.js';
	import { createCredentialFlow } from '../../lib/credential-flow.svelte.js';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();
	const flow = createCredentialFlow('registration');
	afterNavigate((navigation) => {
		flow.sanitizeURL(
			navigation.to?.url,
			(url) => goto(url, { replace: true, shallow: true }),
			() => window.location.href
		);
	});
	let grantAvailable = $derived(data.hasGrant && setupGrant.ready);
	let statusMessage = $derived(
		flow.state.statusMessage ||
			(!data.hasGrant
				? 'This setup link is incomplete. Ask an administrator to issue a new one.'
				: grantAvailable
					? 'Choose a password for the administrator account.'
					: 'Securing your setup link…')
	);
	async function submit(event: SubmitEvent) {
		event.preventDefault();
		const form = event.currentTarget as HTMLFormElement;
		const passwordField = form.elements.namedItem('password') as HTMLInputElement;
		await flow.submit(passwordField);
	}
</script>

<svelte:head>
	<title>Create administrator · Deploy Manager</title>
	<meta name="referrer" content="no-referrer" />
</svelte:head>

<main
	class="mx-auto my-[clamp(2rem,12vh,7rem)] w-[min(100%-2rem,28rem)] rounded-2xl border border-[#e1e6df] bg-white p-[clamp(1.5rem,6vw,2.5rem)] shadow-[0_1.5rem_4rem_rgb(25_44_28_/_8%)]"
>
	<p class="mb-3 text-xs font-bold tracking-[0.12em] text-[#526957] uppercase">Deploy Manager</p>
	<h1 class="m-0 text-[clamp(1.8rem,7vw,2.35rem)] tracking-[-0.04em]">Create administrator</h1>
	<p class="leading-[1.55] text-[#58645b]">
		Set a strong password to secure this deployment manager.
	</p>
	<p class="my-6 min-h-6 leading-6" role="status" aria-live="polite">{statusMessage}</p>
	{#if grantAvailable && !flow.state.complete}
		<form class="grid gap-[0.7rem]" onsubmit={submit}>
			<label class="font-semibold" for="password">Password</label>
			<input
				class="w-full rounded-lg border border-[#aeb9ae] px-3.5 py-3 font-[inherit] focus-visible:outline-3 focus-visible:outline-offset-2 focus-visible:outline-[#82b392]"
				id="password"
				name="password"
				type="password"
				autocomplete="new-password"
				minlength="12"
				required
				disabled={flow.state.pending}
			/>
			<p class="mt-0 mb-2 text-sm leading-[1.55] text-[#58645b]">
				Use at least 12 characters. Your password stays in this browser.
			</p>
			<button
				class="min-h-12 cursor-pointer rounded-lg border-0 bg-[#214c30] font-[inherit] font-semibold text-white focus-visible:outline-3 focus-visible:outline-offset-2 focus-visible:outline-[#82b392] disabled:cursor-wait disabled:opacity-70"
				type="submit"
				disabled={flow.state.pending}
				>{flow.state.pending ? 'Creating account…' : 'Create account'}</button
			>
		</form>
	{/if}
	<a class="mt-6 inline-block text-sm text-[#214c30] underline underline-offset-4" href="/"
		>Return to start</a
	>
</main>
