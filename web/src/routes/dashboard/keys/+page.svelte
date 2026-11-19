<script lang="ts">
	import { sdk } from '#lib/sdk/index.js';
	import type { RepositoryKey } from '#lib/sdk/repository.js';
	import { onMount } from 'svelte';
	let repositoryKeys = $state<RepositoryKey[]>([]);
	let keyName = $state('');
	let privateKeyInput = $state('');
	let keyError = $state('');
	let keyBusy = $state(false);
	async function loadRepositoryKeys() {
		try {
			repositoryKeys = (await sdk.repositoryKeys.list()).data;
		} catch (cause) {
			keyError = cause instanceof Error ? cause.message : 'Could not load repository keys.';
		}
	}
	async function deleteKey(key: RepositoryKey) {
		keyError = '';
		try {
			await sdk.repositoryKeys.delete(key.id);
			await loadRepositoryKeys();
		} catch (cause) {
			keyError =
				cause instanceof Error && cause.message.includes('409')
					? 'This key is assigned to a project. Assign another key before removing it.'
					: cause instanceof Error
						? cause.message
						: 'Could not remove the key.';
		}
	}
	async function saveRepositoryKey(generate: boolean) {
		if (keyBusy) return;
		keyBusy = true;
		keyError = '';
		const name = keyName;
		const privateKey = generate ? '' : privateKeyInput;
		// Remove the secret from reactive state before starting the network request.
		privateKeyInput = '';
		try {
			await sdk.repositoryKeys.create(generate ? { name, generate: true } : { name, privateKey });
			keyName = '';
			await loadRepositoryKeys();
		} catch (cause) {
			keyError = cause instanceof Error ? cause.message : 'Could not save the repository key.';
		} finally {
			// The textarea is cleared before submission; this request-local string remains until the request settles and cannot be securely zeroed.
			keyBusy = false;
		}
	}
	onMount(() => {
		return sdk.resources.subscribe({
			identity: { kind: 'repository_keys' },
			read: () => sdk.repositoryKeys.list().then((result) => result.data),
			onValue: (keys, cause) => {
				if (cause) {
					keyError = cause instanceof Error ? cause.message : 'Could not load repository keys.';
					return;
				}
				repositoryKeys = keys ?? [];
				keyError = '';
			}
		});
	});
</script>

<svelte:head><title>Repository keys · Deploy Manager</title></svelte:head>
<main class="mx-auto w-[min(100%-2rem,64rem)] py-10">
	<h1 class="m-0 text-3xl font-semibold tracking-[-0.03em] text-[#243528]">Repository keys</h1>
	<p class="mt-2 text-sm text-[#58645b]">
		Assign a saved key when connecting an SSH repository in <a
			class="font-semibold text-[#214c30] underline"
			href="/dashboard/projects">Projects</a
		>.
	</p>
	<section class="mt-8 rounded-xl border border-[#e1e6df] bg-white p-5">
		<h2 class="m-0 text-xl font-semibold text-[#243528]">Manage keys</h2>
		<p class="mt-2 text-sm text-[#58645b]">
			Keys let Deploy Manager connect to private repositories. Private keys are encrypted and never
			shown here.
		</p>
		{#if keyError}<p
				role="alert"
				class="mt-4 rounded-lg border border-red-200 bg-red-50 p-3 text-red-800"
			>
				{keyError}
			</p>{/if}
		<form
			class="mt-4 grid gap-4"
			onsubmit={(event) => {
				event.preventDefault();
				void saveRepositoryKey(true);
			}}
		>
			<label class="grid gap-1 text-sm font-medium text-[#354238]"
				>Key name<input
					class="min-h-11 rounded-lg border border-[#aeb9ae] px-3"
					bind:value={keyName}
					required
					maxlength="100"
				/></label
			>
			<button
				class="min-h-11 w-fit rounded-lg bg-[#214c30] px-5 font-semibold text-white disabled:opacity-60"
				disabled={keyBusy}>Create Ed25519 key</button
			>
		</form>
		<form
			class="mt-6 grid gap-4 border-t border-[#e1e6df] pt-5"
			onsubmit={(event) => {
				event.preventDefault();
				void saveRepositoryKey(false);
			}}
		>
			<label class="grid gap-1 text-sm font-medium text-[#354238]"
				>Name for imported key<input
					class="min-h-11 rounded-lg border border-[#aeb9ae] px-3"
					bind:value={keyName}
					required
					maxlength="100"
				/></label
			>
			<label class="grid gap-1 text-sm font-medium text-[#354238]"
				>Private key<textarea
					class="min-h-32 rounded-lg border border-[#aeb9ae] p-3 font-mono text-sm"
					bind:value={privateKeyInput}
					required
					autocomplete="off"
					spellcheck="false"
					aria-describedby="private-key-note"></textarea></label
			>
			<p id="private-key-note" class="-mt-3 text-sm text-[#58645b]">
				Paste an unencrypted OpenSSH or PEM key: Ed25519, RSA (3072 bits or stronger), or ECDSA
				(P-256, P-384, P-521). The text is cleared as soon as you submit.
			</p>
			<button
				class="min-h-11 w-fit rounded-lg border border-[#aeb9ae] px-5 font-semibold text-[#214c30] disabled:opacity-60"
				disabled={keyBusy}>Import private key</button
			>
		</form>
		<h3 class="mt-8 text-lg font-semibold text-[#243528]">Saved keys</h3>
		{#if repositoryKeys.length === 0}<p class="text-[#58645b]">No repository keys yet.</p>{:else}<ul
				class="grid gap-3 p-0"
			>
				{#each repositoryKeys as key (key.id)}<li
						class="list-none rounded-lg border border-[#e1e6df] p-4"
					>
						<p class="m-0 font-semibold text-[#243528]">{key.name}</p>
						<p class="mt-1 text-sm text-[#58645b]">
							{key.algorithm} · {key.bits} bits · {key.fingerprint}
						</p>
						<code class="mt-2 block text-xs break-all text-[#58645b]">{key.publicKey}</code>
						<p class="mt-2 text-xs text-[#58645b]">
							Added {new Date(key.createdAt).toLocaleString()}
						</p>
						<button
							class="mt-3 rounded-lg border border-[#aeb9ae] px-3 py-2 text-sm"
							onclick={() => deleteKey(key)}>Remove key</button
						>
					</li>{/each}
			</ul>{/if}
	</section>
</main>
