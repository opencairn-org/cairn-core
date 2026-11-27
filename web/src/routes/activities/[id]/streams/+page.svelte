<script lang="ts">
	import StreamChart from '#lib/components/StreamChart.svelte';
	import { buildSeries } from '../streamSeries';
	import type { PageData } from './$types';

	let { data }: { data: PageData } = $props();

	const a = $derived(data.activity);
	// Prefer the per-channel-merged stream (best field from best source) when the
	// activity has multiple sources; fall back to the single primary stream.
	const stream = $derived(data.mergedStream ?? data.stream);
	const isMerged = $derived(!!data.mergedStream);
	const provenance = $derived(data.mergedProvenance ?? []);
	const offsets = $derived(stream?.offsets ?? []);
	const series = $derived(buildSeries(stream));

	// One uPlot sync group so the hover cursor aligns across all stacked panels.
	const syncKey = $derived(`streams-${a.id}`);
</script>

<section class="flex h-[calc(100dvh-4rem)] max-md:h-[calc(100dvh-7.5rem)] flex-col gap-3">
	<header class="flex items-center justify-between gap-4">
		<div class="min-w-0">
			<a href={`/activities/${a.id}`} class="text-xs text-accent-400 hover:text-accent-300">
				← {a.title || 'Activity'}
			</a>
			<h1 class="truncate text-xl font-semibold tracking-tight">Streams</h1>
		</div>
	</header>

	{#if isMerged && provenance.length > 0}
		<div class="flex flex-wrap items-center gap-1 text-xs text-zinc-500">
			<span class="rounded bg-accent-500/15 px-1.5 py-0.5 text-accent-300">merged</span>
			<span>best channel from each source:</span>
			{#each provenance as p (p.channel)}
				<span class="rounded bg-zinc-800 px-1.5 py-0.5 text-zinc-300">
					{p.channel} ← {p.provider}
				</span>
			{/each}
		</div>
	{/if}

	{#if series.length > 0}
		<div class="min-h-0 flex-1 space-y-4 overflow-y-auto pr-1">
			{#each series as s (s.label)}
				<div class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-3">
					<div class="mb-1 text-xs font-medium uppercase tracking-wide" style:color={s.color}>
						{s.label}{s.unit ? ` (${s.unit})` : ''}
					</div>
					<StreamChart {offsets} series={[s]} height={180} {syncKey} />
				</div>
			{/each}
		</div>
	{:else}
		<div
			class="flex flex-1 items-center justify-center rounded-lg border border-dashed border-zinc-700 bg-zinc-900/30 text-sm text-zinc-400"
		>
			This activity has no stream data.
		</div>
	{/if}
</section>
