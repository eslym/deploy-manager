<script lang="ts">
	import { goto } from '$app/navigation';
	import { onMount } from 'svelte';
	import { sdk } from '../../lib/sdk/index.js';
	import type { LayoutProps } from './$types';

	let { children }: LayoutProps = $props();
	let busy = $state(false);
	let status = $state('');

	onMount(() => {
		const handleSessionLost = () => {
			const next = `${window.location.pathname}${window.location.search}${window.location.hash}`;
			const query = new URLSearchParams({ next });
			void goto(`/login?${query.toString()}`, { replace: true });
		};
		window.addEventListener('deploy-manager:session-lost', handleSessionLost);
		return () => window.removeEventListener('deploy-manager:session-lost', handleSessionLost);
	});

	async function logout() {
		if (busy) return;
		busy = true;
		status = 'Signing out…';
		try {
			await sdk.auth.logout();
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
<nav
	aria-label="Dashboard"
	class="border-b border-[#e1e6df] bg-white px-[max(1rem,calc((100%-64rem)/2))] py-3"
>
	<div class="flex flex-wrap gap-x-6 gap-y-2 text-sm font-semibold">
		<a class="text-[#214c30] underline underline-offset-4" href="/dashboard/projects">Projects</a>
		<a class="text-[#214c30] underline underline-offset-4" href="/dashboard/keys">Repository keys</a
		>
		<a class="text-[#214c30] underline underline-offset-4" href="/dashboard/ssh-hosts">SSH hosts</a>
	</div>
</nav>
{@render children()}
