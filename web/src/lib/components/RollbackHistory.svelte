<script lang="ts">
	import { onDestroy } from 'svelte';
	import { beforeNavigate } from '$app/navigation';
	import { BrowserSDKError, sdk } from '#lib/sdk/index.js';
	import type { DeploymentHistory, DeploymentHistoryCommit } from '#lib/sdk/deployments.js';

	let {
		deploymentId,
		projectId,
		deploymentName,
		branch,
		busy = $bindable(false),
		onqueued
	}: {
		deploymentId: string;
		projectId: string;
		deploymentName: string;
		branch: string;
		busy?: boolean;
		onqueued: () => void | Promise<void>;
	} = $props();
	const uid = $props.id();
	let open = $state(false);
	let search = $state('');
	let activeSearch = $state('');
	let history = $state.raw<DeploymentHistory | null>(null);
	let loading = $state(false);
	let historyError = $state('');
	let rollbackError = $state('');
	let queued = $state('');
	let selected = $state.raw<DeploymentHistoryCommit | null>(null);
	let permanent = $state(false);
	let requestedPage = $state(1);
	let requestedSearch = $state('');
	let requestController: AbortController | null = null;
	let historySubscription: (() => void) | null = null;
	let requestVersion = 0;
	let latestHistoryQuery: {
		deploymentId: string;
		search: string;
		page: number;
		limit: number;
	} | null = null;
	let destroyed = false;
	let historyDeploymentId = $state('');
	const historyLimit = 20;

	function isLatestHistoryQuery(query: NonNullable<typeof latestHistoryQuery>, version: number) {
		return (
			version === requestVersion &&
			latestHistoryQuery?.deploymentId === query.deploymentId &&
			latestHistoryQuery.search === query.search &&
			latestHistoryQuery.page === query.page &&
			latestHistoryQuery.limit === query.limit
		);
	}

	beforeNavigate(() => {
		historySubscription?.();
		historySubscription = null;
		requestController?.abort();
	});

	onDestroy(() => {
		destroyed = true;
		requestController?.abort();
	});

	function loadHistory(page = requestedPage, query = activeSearch) {
		if (busy) return;
		requestedPage = page;
		requestedSearch = query;
		activeSearch = query;
		selected = null;
		rollbackError = '';
		sdk.resources.refresh({
			kind: 'history',
			projectId,
			deploymentId,
			search: query,
			page,
			limit: historyLimit
		});
	}

	$effect(() => {
		if (!open) return;
		const queryIdentity = {
			projectId,
			deploymentId,
			search: requestedSearch,
			page: requestedPage,
			limit: historyLimit
		};
		requestController?.abort();
		const controller = new AbortController();
		requestController = controller;
		const version = ++requestVersion;
		latestHistoryQuery = queryIdentity;
		loading = true;
		historyError = '';
		const dispose = sdk.resources.subscribe({
			identity: { kind: 'history', ...queryIdentity },
			read: async () =>
				(
					await sdk.deployments.history(
						deploymentId,
						{ search: queryIdentity.search, page: queryIdentity.page, limit: queryIdentity.limit },
						controller.signal
					)
				).data,
			onValue: (value, cause) => {
				if (destroyed || controller.signal.aborted || !isLatestHistoryQuery(queryIdentity, version))
					return;
				if (cause) {
					historyError =
						'Could not load commit history. Try again. If the branch has not been fetched yet, refresh branches and tags first.';
				} else if (value) {
					history = value;
					historyDeploymentId = deploymentId;
					historyError = '';
				}
				loading = false;
			}
		});
		historySubscription = dispose;
		return () => {
			dispose();
			if (historySubscription === dispose) historySubscription = null;
			controller.abort();
			if (requestController === controller) requestController = null;
		};
	});

	function toggle() {
		open = !open;
		if (!open) selected = null;
	}

	function submitSearch(event: SubmitEvent) {
		event.preventDefault();
		void loadHistory(1, search.trim());
	}

	async function rollback() {
		if (busy || loading || !selected) return;
		const commit = selected;
		busy = true;
		rollbackError = '';
		queued = '';
		try {
			const outcome = await sdk.deployments.rollback(deploymentId, {
				commit: commit.id,
				permanent
			});
			if (outcome.code !== 'deployment.rollback_queued') {
				throw new Error(
					'Could not confirm whether rollback was accepted. Check Latest deployment before trying again.'
				);
			}
			const accepted = outcome.data;
			if (!accepted.jobId || accepted.status !== 'queued') {
				throw new Error(
					'Could not confirm the rollback request. Check Latest deployment before trying again.'
				);
			}
			queued = permanent
				? `Permanent rollback to ${commit.shortId} queued. The branch is preserved and automatic deployment is paused now, even if the rollback fails. Follow progress in Latest deployment.`
				: `Rollback to ${commit.shortId} queued. Follow its progress in Latest deployment.`;
			selected = null;
		} catch (cause) {
			rollbackError =
				cause instanceof BrowserSDKError &&
				(cause.outcome.kind !== 'server' || cause.outcome.status >= 500)
					? 'Could not confirm whether rollback was accepted. Check Latest deployment before trying again.'
					: cause instanceof Error
						? cause.message
						: 'Could not queue rollback.';
		} finally {
			busy = false;
		}
		if (queued) await onqueued();
	}
</script>

<div class="mt-3">
	<button
		type="button"
		class="min-h-9 rounded-lg border border-[#aeb9ae] px-3 text-sm font-semibold text-[#214c30] disabled:opacity-60"
		aria-expanded={open}
		aria-controls={`${uid}-history`}
		onclick={toggle}
		disabled={busy}>Rollback</button
	>
	{#if open}
		<section
			id={`${uid}-history`}
			class="mt-3 rounded-lg border border-[#e1e6df] p-3"
			aria-label={`Rollback history for ${deploymentName}`}
		>
			<h4 class="m-0 font-semibold text-[#243528]">Choose a commit</h4>
			<p class="mt-1 text-sm text-[#58645b]">
				First-parent history of {branch} from the last fetch.
			</p>
			<form class="mt-3 flex flex-wrap items-end gap-2" onsubmit={submitSearch}>
				<label class="grid min-w-48 flex-1 gap-1 text-sm font-medium text-[#354238]">
					Search commits
					<input
						type="search"
						class="min-h-10 rounded-lg border border-[#aeb9ae] px-3"
						bind:value={search}
						disabled={loading || busy}
					/>
				</label>
				<button
					class="min-h-10 rounded-lg border border-[#aeb9ae] px-3 text-sm font-semibold text-[#214c30] disabled:opacity-60"
					disabled={loading || busy}>Search</button
				>
			</form>
			{#if historyError}
				<p role="alert" class="mt-3 text-sm text-red-800">{historyError}</p>
				<button
					type="button"
					class="mt-2 min-h-9 rounded-lg border border-[#aeb9ae] px-3 text-sm font-semibold text-[#214c30] disabled:opacity-60"
					onclick={() => loadHistory(requestedPage, requestedSearch)}
					disabled={loading || busy}>Retry history</button
				>
			{/if}
			{#if loading}<p role="status" class="mt-3 text-sm text-[#58645b]">Loading commits…</p>{/if}
			{#if history && historyDeploymentId === deploymentId && !loading}
				{#if activeSearch}<p class="mt-3 text-sm text-[#58645b]">
						Results for “{activeSearch}”
					</p>{/if}
				<ul class="mt-3 grid gap-2 p-0">
					{#each history.commits as commit (commit.id)}
						<li
							class="flex list-none flex-wrap items-start justify-between gap-2 rounded-lg bg-[#f4f7f3] p-3"
						>
							<div class="min-w-0 flex-1">
								<p class="m-0 text-sm font-medium break-words text-[#243528]">
									<code title={commit.id}>{commit.shortId}</code> · {commit.subject}
								</p>
								<p class="mt-1 text-sm break-words text-[#58645b]">
									{commit.author} ·
									<time datetime={commit.date}>{new Date(commit.date).toLocaleString()}</time>
								</p>
							</div>
							<button
								type="button"
								class="min-h-9 rounded-lg border border-[#aeb9ae] px-3 text-sm font-semibold text-[#214c30] disabled:opacity-60"
								aria-label={`Roll back to ${commit.shortId}`}
								onclick={() => {
									selected = commit;
									permanent = false;
									rollbackError = '';
									queued = '';
								}}
								disabled={busy || !!historyError}>Roll back</button
							>
						</li>
					{:else}
						<li class="list-none py-2 text-sm text-[#58645b]">
							{activeSearch
								? 'No commits match your search. Try a different search.'
								: 'No commits in the cached history. Refresh branches and tags, then search again.'}
						</li>
					{/each}
				</ul>
				<nav
					aria-label={`Commit history pages for ${deploymentName}`}
					class="mt-3 flex items-center gap-3 text-sm"
				>
					<button
						type="button"
						class="min-h-9 rounded-lg border border-[#aeb9ae] px-3 font-semibold text-[#214c30] disabled:opacity-60"
						onclick={() => loadHistory(history!.page - 1)}
						disabled={busy || history.page <= 1}>Previous</button
					>
					<span>Page {history.page}</span>
					<button
						type="button"
						class="min-h-9 rounded-lg border border-[#aeb9ae] px-3 font-semibold text-[#214c30] disabled:opacity-60"
						onclick={() => loadHistory(history!.page + 1)}
						disabled={busy || !history.hasMore}>Next</button
					>
				</nav>
			{/if}
			{#if selected}
				<div
					class="mt-3 rounded-lg border border-amber-200 bg-amber-50 p-3"
					aria-label="Choose rollback behavior"
				>
					<p class="m-0 text-sm text-[#243528]">
						Roll back “{deploymentName}” to <code>{selected.shortId}</code> — {selected.subject}?
					</p>
					<fieldset class="mt-3 grid gap-2 text-sm text-[#354238]">
						<legend class="font-semibold">After rollback</legend>
						<label class="flex items-start gap-2">
							<input
								type="radio"
								name={`${uid}-rollback-type`}
								bind:group={permanent}
								value={false}
								disabled={busy}
							/>
							<span
								><strong>Temporary rollback</strong> — automatic deployments stay on. A later Deploy or
								push can replace this version.</span
							>
						</label>
						<label class="flex items-start gap-2">
							<input
								type="radio"
								name={`${uid}-rollback-type`}
								bind:group={permanent}
								value={true}
								disabled={busy}
							/>
							<span
								><strong>Permanent rollback</strong> — automatic deployments pause and the selected branch
								stays. The pause remains even if the rollback fails.</span
							>
						</label>
					</fieldset>
					<div class="mt-3 flex flex-wrap gap-2">
						<button
							type="button"
							class="min-h-9 rounded-lg bg-[#214c30] px-3 text-sm font-semibold text-white disabled:opacity-60"
							onclick={rollback}
							disabled={busy}>Confirm {permanent ? 'permanent' : 'temporary'} rollback</button
						>
						<button
							type="button"
							class="min-h-9 rounded-lg border border-[#aeb9ae] px-3 text-sm font-semibold disabled:opacity-60"
							onclick={() => {
								selected = null;
								rollbackError = '';
							}}
							disabled={busy}>Cancel</button
						>
					</div>
				</div>
			{/if}
			{#if rollbackError}<p role="alert" class="mt-3 text-sm text-red-800">{rollbackError}</p>{/if}
			{#if queued}<p role="status" class="mt-3 text-sm text-[#214c30]">{queued}</p>{/if}
		</section>
	{/if}
</div>
