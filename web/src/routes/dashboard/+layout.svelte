<script lang="ts">
	import { goto } from '$app/navigation';
	import type { LayoutProps } from './$types';

	let { children }: LayoutProps = $props();
	let busy = $state(false);
	let status = $state('');

	async function logout() {
		if (busy) return;
		busy = true;
		status = 'Signing out…';
		try {
			const response = await fetch('/api/logout', {
				method: 'POST',
				credentials: 'same-origin'
			});
			if (!response.ok) throw new Error('Sign-out request failed');
			await goto('/login', { replaceState: true });
		} catch {
			status = 'Could not sign out. Check your connection and try again.';
			busy = false;
		}
	}
</script>

<svelte:head>
	<title>Dashboard · Deploy Manager</title>
</svelte:head>

<header
	class="flex items-center justify-between border-b border-[#e1e6df] bg-white px-[max(1rem,calc((100%-64rem)/2))] py-4"
>
	<a class="font-bold tracking-[-0.02em] text-[#214c30] no-underline" href="/dashboard"
		>Deploy Manager</a
	>
	<div class="flex items-center gap-4">
		<a
			class="text-sm font-semibold text-[#214c30] underline underline-offset-4"
			href="/account/password">Change password</a
		>
		<span class="sr-only" role="status" aria-live="polite">{status}</span>
		<button
			class="min-h-10 rounded-lg border border-[#aeb9ae] bg-white px-4 font-[inherit] font-semibold text-[#214c30] focus-visible:outline-3 focus-visible:outline-offset-2 focus-visible:outline-[#82b392] disabled:opacity-60"
			onclick={logout}
			disabled={busy}>{busy ? 'Signing out…' : 'Sign out'}</button
		>
	</div>
</header>
{@render children()}
