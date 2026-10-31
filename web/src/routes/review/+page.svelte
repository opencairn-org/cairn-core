<script lang="ts">
	import { onMount } from 'svelte';
	import SportIcon from '#lib/components/SportIcon.svelte';
	import { formatDateOnly } from '#lib/format.js';
	import { m } from '#lib/paraglide/messages.js';

	// Confidence-band review queue (brief §7.3): medium-confidence merges the
	// matcher flagged. The user confirms ("looks right" → clear flag) or opens
	// the activity to split it. Possible duplicates (kind "duplicate") are
	// merged or kept apart for good.
	type Item = {
		id: string;
		title: string;
		type: string;
		start_time: string;
		match_confidence: string;
		kind?: string;
		duplicate_of?: string;
	};

	const itemKey = (i: Item) => `${i.id}:${i.duplicate_of ?? ''}`;

	let items = $state<Item[]>([]);
	let loaded = $state(false);
	let error = $state<string | null>(null);
	let busy = $state<string | null>(null);

	async function load() {
		try {
			const res = await fetch('/api/review-queue');
			if (!res.ok) throw new Error(await res.text());
			items = (await res.json()).items ?? [];
		} catch (e) {
			error = (e as Error).message || 'failed to load';
		}
		loaded = true;
	}
	onMount(load);

	async function post(it: Item, url: string, body: object) {
		const key = itemKey(it);
		busy = key;
		try {
			const res = await fetch(url, {
				method: 'POST',
				headers: { 'content-type': 'application/json' },
				body: JSON.stringify(body)
			});
			if (res.ok) items = items.filter((i) => itemKey(i) !== key);
		} finally {
			busy = null;
		}
	}

	const confirm = (it: Item) => post(it, `/api/activities/${it.id}/reviewed`, {});
	const keepBoth = (it: Item) =>
		post(it, `/api/activities/${it.id}/reviewed`, { duplicate_of: it.duplicate_of });
	const merge = (it: Item) =>
		post(it, '/api/activities/merge', { activity_a: it.duplicate_of, activity_b: it.id });
</script>

<svelte:head><title>Review · Cairn</title></svelte:head>

<div class="mx-auto max-w-3xl px-4 py-6">
	<h1 class="mb-1 text-xl font-semibold text-zinc-100">Review queue</h1>
	<p class="mb-6 text-sm text-zinc-500">
		Activities the merge engine combined from multiple sources with only medium confidence. Confirm
		the ones that look right, or open an activity to split a wrong merge.
	</p>

	{#if !loaded}
		<p class="text-sm text-zinc-600">Loading…</p>
	{:else if error}
		<p class="text-sm text-red-400">{error}</p>
	{:else if items.length === 0}
		<p class="text-sm text-zinc-500">Nothing to review — all merges are high-confidence. 🎉</p>
	{:else}
		<ul class="space-y-2">
			{#each items as it (itemKey(it))}
				<li
					class="flex items-center gap-3 rounded-lg border border-zinc-800 bg-zinc-900/40 p-3 max-md:flex-wrap"
				>
					<SportIcon type={it.type} size={18} />
					<div class="min-w-0 flex-1">
						<a
							href={`/activities/${it.id}`}
							class="truncate font-medium text-zinc-200 hover:text-accent-300"
						>
							{it.title || 'Untitled activity'}
						</a>
						<div class="text-xs text-zinc-600">{formatDateOnly(it.start_time)}</div>
						{#if it.kind === 'duplicate'}
							<div class="text-xs text-zinc-500">
								{m.review_dup_hint()}
								<a href={`/activities/${it.duplicate_of}`} class="hover:text-accent-300"
									>{m.review_dup_other()}</a
								>
							</div>
						{/if}
					</div>
					{#if it.kind === 'duplicate'}
						<span class="rounded bg-sky-500/15 px-1.5 py-0.5 text-xs text-sky-300"
							>{m.review_dup_label()}</span
						>
						<button
							type="button"
							onclick={() => keepBoth(it)}
							disabled={busy === itemKey(it)}
							class="rounded border border-zinc-700 px-2 py-1 text-xs text-zinc-300 hover:border-accent-500 disabled:opacity-50"
						>
							{m.review_dup_keep()}
						</button>
						<button
							type="button"
							onclick={() => merge(it)}
							disabled={busy === itemKey(it)}
							class="rounded bg-accent-600 px-2 py-1 text-xs font-medium text-white hover:bg-accent-500 disabled:opacity-50"
						>
							{busy === itemKey(it) ? '…' : m.review_dup_merge()}
						</button>
					{:else}
						<span class="rounded bg-amber-500/15 px-1.5 py-0.5 text-xs text-amber-300">medium</span>
						<a
							href={`/activities/${it.id}/manage`}
							class="rounded border border-zinc-700 px-2 py-1 text-xs text-zinc-300 hover:border-accent-500"
						>
							Inspect
						</a>
						<button
							type="button"
							onclick={() => confirm(it)}
							disabled={busy === itemKey(it)}
							class="rounded bg-accent-600 px-2 py-1 text-xs font-medium text-white hover:bg-accent-500 disabled:opacity-50"
						>
							{busy === itemKey(it) ? '…' : 'Looks right'}
						</button>
					{/if}
				</li>
			{/each}
		</ul>
	{/if}
</div>
