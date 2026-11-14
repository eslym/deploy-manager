<script lang="ts">
	type Commit = { id: string; shortId: string; subject: string; author: string; date: string };
	type History = { commits: Commit[]; page: number; hasMore: boolean };
	let {
		deploymentId,
		deploymentName,
		branch,
		busy = $bindable(false),
		onqueued
	}: {
		deploymentId: string;
		deploymentName: string;
		branch: string;
		busy?: boolean;
		onqueued: () => void | Promise<void>;
	} = $props();
	const uid = $props.id();
	let open = $state(false);
	let search = $state('');
	let activeSearch = $state('');
	let history = $state.raw<History | null>(null);
	let loading = $state(false);
	let historyError = $state('');
	let rollbackError = $state('');
	let queued = $state('');
	let selected = $state.raw<Commit | null>(null);
	let requestedPage = 1;
	let requestedSearch = '';

	async function loadHistory(page = 1, query = activeSearch) {
		if (loading || busy) return;
		loading = true;
		historyError = '';
		selected = null;
		rollbackError = '';
		requestedPage = page;
		requestedSearch = query;
		try {
			const params = new URLSearchParams({ search: query, page: String(page), limit: '20' });
			const response = await fetch(`/api/deployments/${deploymentId}/history?${params}`, {
				credentials: 'same-origin'
			});
			if (!response.ok) throw new Error('Could not load commit history. Try again.');
			history = (await response.json()) as History;
			activeSearch = query;
		} catch {
			historyError =
				'Could not load commit history. Try again. If the branch has not been fetched yet, refresh branches and tags first.';
		} finally {
			loading = false;
		}
	}

	function toggle() {
		open = !open;
		if (open && !history && !loading) void loadHistory();
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
			const response = await fetch(`/api/deployments/${deploymentId}/rollback`, {
				method: 'POST',
				credentials: 'same-origin',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ commit: commit.id })
			});
			if (!response.ok) {
				throw new Error(
					response.status >= 500
						? 'Could not queue rollback. Try again shortly.'
						: 'Rollback was rejected. Refresh branches and tags, then search the history again before retrying.'
				);
			}
			// Complete the accepted request before refreshing deployment state.
			const accepted: unknown = await response.json().catch(() => null);
			if (
				!accepted ||
				typeof accepted !== 'object' ||
				!('jobId' in accepted) ||
				typeof accepted.jobId !== 'string' ||
				!accepted.jobId.trim()
			) {
				throw new Error(
					'Could not confirm the rollback request. Check Latest deployment before retrying.'
				);
			}
			queued = `Rollback to ${commit.shortId} queued. Follow its progress in Latest deployment.`;
			selected = null;
		} catch (cause) {
			rollbackError =
				cause instanceof Error ? cause.message : 'Could not queue rollback. Try again.';
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
		disabled={busy}>Temporary rollback</button
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
			{#if history && !loading}
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
					aria-label="Confirm temporary rollback"
				>
					<p class="m-0 text-sm text-[#243528]">
						Roll back “{deploymentName}” to <code>{selected.shortId}</code> — {selected.subject}?
					</p>
					<p class="mt-2 text-sm text-[#354238]">
						The configured branch and automatic deployment remain unchanged. Deploy may replace this
						rollback with the latest branch commit; matching webhooks may do so when automatic
						deployment is enabled.
					</p>
					<div class="mt-3 flex flex-wrap gap-2">
						<button
							type="button"
							class="min-h-9 rounded-lg bg-[#214c30] px-3 text-sm font-semibold text-white disabled:opacity-60"
							onclick={rollback}
							disabled={busy}>Confirm temporary rollback</button
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
