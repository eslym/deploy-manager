<script lang="ts">
	import { onMount } from 'svelte';
	import sdk from '#lib/sdk/index.js';
	let { projectId, provider }: { projectId: string; provider: 'github' | 'gitea' } = $props();
	const uid = $props.id();
	let providerName = $derived(provider === 'github' ? 'GitHub' : 'Gitea');
	type Settings = { configured: boolean; url: string };
	let settings = $state<Settings | null>(null);
	let secret = $state('');
	let actionError = $state('');
	let settingsError = $state('');
	let busy = $state(false);
	let loading = $state(true);
	let copied = $state('');
	let loadVersion = 0;

	async function load(id: string, version: number) {
		loading = true;
		settingsError = '';
		try {
			const outcome = await sdk.webhooks.settings(id, provider);
			const result: Settings = outcome.data;
			if (version === loadVersion && id === projectId)
				settings = { ...result, url: new URL(result.url, window.location.origin).toString() };
		} catch (cause) {
			if (version === loadVersion && id === projectId)
				settingsError = cause instanceof Error ? cause.message : 'Could not load webhook settings.';
		} finally {
			if (version === loadVersion && id === projectId) loading = false;
		}
	}

	async function rotate() {
		if (busy) return;
		if (
			settings?.configured &&
			!window.confirm(
				`Create a new secret? ${providerName} deliveries signed with the current secret will stop working until you update the webhook.`
			)
		)
			return;
		busy = true;
		const id = projectId;
		const version = loadVersion;
		secret = '';
		copied = '';
		actionError = '';
		try {
			const outcome = await sdk.webhooks.rotate(id, provider);
			const result: { url: string; secret: string } = outcome.data;
			if (version !== loadVersion || id !== projectId) return;
			secret = result.secret;
			settings = { configured: true, url: new URL(result.url, window.location.origin).toString() };
		} catch (cause) {
			if (version === loadVersion && id === projectId)
				actionError =
					cause instanceof Error ? cause.message : 'Could not create the webhook secret.';
		} finally {
			if (version === loadVersion && id === projectId) busy = false;
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
		const id = projectId;
		const version = ++loadVersion;
		const dispose = sdk.resources.subscribe({
			identity: { kind: 'webhook_settings', projectId: id },
			read: async () => {
				const [github, gitea] = await Promise.allSettled([
					sdk.webhooks.settings(id, 'github').then((outcome) => outcome.data),
					sdk.webhooks.settings(id, 'gitea').then((outcome) => outcome.data)
				]);
				return { github, gitea };
			},
			onValue: (results, cause) => {
				if (version !== loadVersion || id !== projectId) return;
				if (cause) {
					settingsError =
						cause instanceof Error ? cause.message : 'Could not load webhook settings.';
					loading = false;
					return;
				}
				const providerResult = results![provider];
				if (providerResult.status === 'rejected') {
					settingsError =
						providerResult.reason instanceof Error
							? providerResult.reason.message
							: 'Could not load webhook settings.';
					loading = false;
					return;
				}
				const result = providerResult.value;
				settings = { ...result, url: new URL(result.url, window.location.origin).toString() };
				settingsError = '';
				loading = false;
			}
		});
		return () => {
			dispose();
			loadVersion++;
			secret = '';
		};
	});
</script>

<section
	class="mt-8 rounded-xl border border-[#e1e6df] bg-white p-5"
	aria-labelledby={`${uid}-webhook-heading`}
>
	<h2 id={`${uid}-webhook-heading`} class="m-0 text-xl font-semibold text-[#243528]">
		{providerName} webhook
	</h2>
	<p class="mt-2 text-sm text-[#58645b]">
		Deployments following a branch can update automatically when {providerName} pushes to it.
	</p>
	{#if actionError}<p
			role="alert"
			class="mt-3 rounded-lg border border-red-200 bg-red-50 p-3 text-red-800"
		>
			{actionError}
		</p>{/if}
	{#if settingsError}<p
			role="alert"
			class="mt-3 rounded-lg border border-red-200 bg-red-50 p-3 text-red-800"
		>
			{settingsError}
		</p>{/if}
	{#if settingsError && !settings}
		<button
			class="mt-3 rounded-lg border border-[#aeb9ae] px-4 py-2 font-semibold disabled:opacity-60"
			onclick={() => void load(projectId, ++loadVersion)}
			disabled={loading}
		>
			{loading ? 'Loading settings…' : 'Retry loading settings'}
		</button>
	{/if}
	{#if loading && !settings}<p class="mt-3 text-sm text-[#58645b]" role="status">
			Loading webhook settings…
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
			disabled={busy || loading}>{settings.configured ? 'Rotate secret' : 'Set up webhook'}</button
		>
	{/if}
	{#if secret && settings}
		{@const payloadURL = settings.url}
		<div
			class="mt-4 rounded-lg border border-amber-300 bg-amber-50 p-4"
			aria-labelledby={`${uid}-webhook-setup-heading`}
		>
			<h3 id={`${uid}-webhook-setup-heading`} class="m-0 font-semibold text-amber-950">
				Add this webhook in {providerName}
			</h3>
			<p class="mt-2 text-sm text-amber-950">
				Use these settings in the repository’s webhook page. {providerName} will send push events only.
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
