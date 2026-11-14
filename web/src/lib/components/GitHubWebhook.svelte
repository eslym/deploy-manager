<script lang="ts">
	import { onMount } from 'svelte';
	let { projectId }: { projectId: string } = $props();
	type Settings = { configured: boolean; url: string };
	let settings = $state<Settings | null>(null);
	let secret = $state('');
	let error = $state('');
	let busy = $state(false);
	let copied = $state('');
	let loadVersion = 0;

	async function request(path: string, init?: RequestInit) {
		const response = await fetch(path, {
			...init,
			credentials: 'same-origin',
			headers: { 'Content-Type': 'application/json', ...init?.headers }
		});
		if (!response.ok) {
			if (response.status === 401 || response.status === 403)
				throw new Error('Your session may have expired. Sign in again.');
			throw new Error(
				response.status >= 500
					? 'GitHub webhook settings could not be saved. Try again shortly.'
					: 'The request was rejected. Refresh and try again.'
			);
		}
		return response.status === 204 ? null : response.json();
	}

	async function load(id: string, version: number) {
		try {
			const result: Settings = await request(`/api/projects/${id}/github-webhook`);
			if (version === loadVersion && id === projectId)
				settings = { ...result, url: new URL(result.url, window.location.origin).toString() };
		} catch (cause) {
			if (version === loadVersion && id === projectId)
				error = cause instanceof Error ? cause.message : 'Could not load webhook settings.';
		}
	}

	async function rotate() {
		if (busy) return;
		if (
			settings?.configured &&
			!window.confirm(
				'Create a new secret? GitHub deliveries signed with the current secret will stop working until you update the webhook.'
			)
		)
			return;
		busy = true;
		secret = '';
		error = '';
		try {
			const result: { url: string; secret: string } = await request(
				`/api/projects/${projectId}/github-webhook`,
				{ method: 'POST', body: JSON.stringify({ rotate: true }) }
			);
			secret = result.secret;
			settings = { configured: true, url: new URL(result.url, window.location.origin).toString() };
		} catch (cause) {
			error = cause instanceof Error ? cause.message : 'Could not create the webhook secret.';
		} finally {
			busy = false;
		}
	}

	async function copy(value: string, label: string) {
		try {
			await navigator.clipboard.writeText(value);
			copied = `${label} copied`;
		} catch {
			copied = 'Copy was unavailable';
		}
	}

	onMount(() => {
		void load(projectId, ++loadVersion);
	});
</script>

<section
	class="mt-8 rounded-xl border border-[#e1e6df] bg-white p-5"
	aria-labelledby="github-webhook-heading"
>
	<h2 id="github-webhook-heading" class="m-0 text-xl font-semibold text-[#243528]">
		GitHub webhook
	</h2>
	<p class="mt-2 text-sm text-[#58645b]">
		Deployments following a branch can update automatically when GitHub pushes to it.
	</p>
	{#if error}<p
			role="alert"
			class="mt-3 rounded-lg border border-red-200 bg-red-50 p-3 text-red-800"
		>
			{error}
		</p>{/if}
	{#if settings}
		<p class="mt-3 text-sm text-[#354238]">
			{settings.configured
				? 'A signing secret is set for this project.'
				: 'No webhook is set up yet.'}
		</p>
		<button
			class="mt-3 min-h-10 rounded-lg bg-[#214c30] px-4 font-semibold text-white disabled:opacity-60"
			onclick={rotate}
			disabled={busy}>{settings.configured ? 'Rotate secret' : 'Set up webhook'}</button
		>
	{/if}
	{#if secret && settings}
		{@const payloadURL = settings.url}
		<div
			class="mt-4 rounded-lg border border-amber-300 bg-amber-50 p-4"
			aria-labelledby="webhook-setup-heading"
		>
			<h3 id="webhook-setup-heading" class="m-0 font-semibold text-amber-950">
				Add this webhook in GitHub
			</h3>
			<p class="mt-2 text-sm text-amber-950">
				Use these settings in the repository’s webhook page. GitHub will send push events only.
			</p>
			<dl class="mt-3 grid gap-3 text-sm">
				<div>
					<dt class="font-semibold text-[#354238]">Payload URL</dt>
					<dd class="mt-1 flex flex-wrap items-center gap-2">
						<code class="break-all">{payloadURL}</code>
						<button
							class="rounded border border-[#aeb9ae] px-2 py-1"
							onclick={() => copy(payloadURL, 'URL')}>Copy</button
						>
					</dd>
				</div>
				<div>
					<dt class="font-semibold text-[#354238]">Content type</dt>
					<dd class="mt-1"><code>application/json</code></dd>
				</div>
				<div>
					<dt class="font-semibold text-[#354238]">Secret</dt>
					<dd class="mt-1 flex flex-wrap items-center gap-2">
						<code class="break-all">{secret}</code>
						<button
							class="rounded border border-[#aeb9ae] px-2 py-1"
							onclick={() => copy(secret, 'Secret')}>Copy</button
						>
					</dd>
				</div>
			</dl>
			<p class="mt-3 text-sm font-medium text-amber-950">
				Copy the secret now. It won’t be shown again.
			</p>
			{#if copied}<p role="status" class="mt-2 text-sm text-[#214c30]">{copied}</p>{/if}
			<button
				class="mt-3 rounded-lg border border-[#aeb9ae] px-3 py-2 text-sm font-semibold"
				onclick={() => (secret = '')}>Done</button
			>
		</div>
	{/if}
</section>
