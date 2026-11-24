<script lang="ts">
	import { onMount } from 'svelte';
	import SportIcon from '#lib/components/SportIcon.svelte';
	import { formatDistance, formatDuration, formatRelativeDate } from '#lib/format.js';

	type Stats = {
		segments: number;
		efforts: number;
		prs: number;
		crs: number;
		external: number;
		native: number;
	};
	type Segment = {
		id: string;
		name: string;
		activity_type: string;
		source: string;
		distance_m: number;
		elevation_gain_m: number | null;
		avg_grade: number | null;
		effort_count: number;
		best_elapsed_s: number;
		last_effort_at: string;
		has_pr: boolean;
		has_cr: boolean;
	};

	let stats = $state<Stats | null>(null);
	let segments = $state<Segment[]>([]);
	let offset = $state(0);
	let hasMore = $state(false);
	let loading = $state(false);
	let error = $state<string | null>(null);
	let typeFilter = $state('');

	async function load(reset: boolean) {
		loading = true;
		error = null;
		try {
			const off = reset ? 0 : offset;
			// eslint-disable-next-line svelte/prefer-svelte-reactivity -- local query builder
			const params = new URLSearchParams({ limit: '50', offset: String(off) });
			if (typeFilter) params.set('type', typeFilter);
			const res = await fetch(`/api/segments?${params}`);
			if (!res.ok) throw new Error((await res.text()).trim());
			const body = await res.json();
			if (off === 0) {
				stats = body.stats;
				segments = body.segments ?? [];
			} else {
				segments = [...segments, ...(body.segments ?? [])];
			}
			hasMore = body.has_more;
			offset = off + (body.segments?.length ?? 0);
		} catch (e) {
			error = (e as Error).message;
		} finally {
			loading = false;
		}
	}
	onMount(() => load(true));

	function setFilter(t: string) {
		typeFilter = t;
		load(true);
	}

	// Distinct activity types present, for the filter chips.
	const types = $derived([...new Set(segments.map((s) => s.activity_type))].sort());
</script>

<div class="space-y-8">
	<header>
		<h1 class="text-2xl font-semibold tracking-tight">Segments</h1>
		<p class="mt-1 text-sm text-zinc-400">
			Segments you've ridden or run, ranked by how often you've taken them on.
		</p>
	</header>

	{#if error}
		<div class="rounded-lg border border-red-900/50 bg-red-950/30 p-4 text-sm text-red-300">
			{error}
		</div>
	{/if}

	{#if stats}
		<dl class="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-6">
			{#each [{ label: 'Segments', value: stats.segments }, { label: 'Efforts', value: stats.efforts }, { label: 'PRs', value: stats.prs }, { label: 'Course records', value: stats.crs }, { label: 'External', value: stats.external }, { label: 'Native', value: stats.native }] as c (c.label)}
				<div class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-4">
					<dt class="text-xs uppercase tracking-wide text-zinc-500">{c.label}</dt>
					<dd class="mt-1 text-2xl font-semibold tabular-nums">{c.value.toLocaleString()}</dd>
				</div>
			{/each}
		</dl>
	{/if}

	{#if types.length > 1 || typeFilter}
		<div class="flex flex-wrap gap-2">
			<button
				type="button"
				onclick={() => setFilter('')}
				class="rounded border px-2.5 py-1 text-xs {typeFilter === ''
					? 'border-accent-500 bg-accent-500/15 text-accent-200'
					: 'border-zinc-700 text-zinc-400 hover:border-zinc-600'}"
			>
				All
			</button>
			{#each types as t (t)}
				<button
					type="button"
					onclick={() => setFilter(t)}
					class="rounded border px-2.5 py-1 text-xs capitalize {typeFilter === t
						? 'border-accent-500 bg-accent-500/15 text-accent-200'
						: 'border-zinc-700 text-zinc-400 hover:border-zinc-600'}"
				>
					{t}
				</button>
			{/each}
		</div>
	{/if}

	<section>
		<h2 class="mb-3 text-sm font-medium uppercase tracking-wide text-zinc-400">Most attempted</h2>
		{#if segments.length > 0}
			<ul class="divide-y divide-zinc-800 rounded-lg border border-zinc-800">
				{#each segments as s (s.id)}
					<li>
						<a
							href={`/segment/${s.id}`}
							class="flex items-center gap-4 px-4 py-3 transition-colors hover:bg-zinc-900/60"
						>
							<span
								class="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-zinc-800 text-zinc-300"
								title={s.activity_type}
							>
								<SportIcon type={s.activity_type} size={18} />
							</span>
							<div class="min-w-0 flex-1">
								<div class="flex items-center gap-2">
									<span class="truncate font-medium text-zinc-100">{s.name}</span>
									{#if s.has_cr}
										<span
											class="rounded bg-amber-500/15 px-1.5 py-0.5 text-[10px] uppercase text-amber-300"
										>
											CR
										</span>
									{:else if s.has_pr}
										<span
											class="rounded bg-emerald-500/15 px-1.5 py-0.5 text-[10px] uppercase text-emerald-300"
										>
											PR
										</span>
									{/if}
								</div>
								<div class="mt-0.5 text-xs text-zinc-500">
									{formatDistance(s.distance_m)}
									{#if s.avg_grade != null}· {s.avg_grade.toFixed(1)}%{/if}
									· last {formatRelativeDate(s.last_effort_at)}
								</div>
							</div>
							<div class="shrink-0 text-right">
								<div class="font-semibold tabular-nums text-zinc-100">
									{formatDuration(s.best_elapsed_s)}
								</div>
								<div class="text-xs text-zinc-500">
									{s.effort_count} effort{s.effort_count === 1 ? '' : 's'}
								</div>
							</div>
						</a>
					</li>
				{/each}
			</ul>
			{#if hasMore}
				<div class="mt-4 text-center">
					<button
						type="button"
						disabled={loading}
						onclick={() => load(false)}
						class="rounded border border-zinc-700 px-4 py-2 text-sm text-zinc-300 hover:border-accent-500 hover:text-accent-300 disabled:opacity-50"
					>
						{loading ? 'Loading…' : 'Load more'}
					</button>
				</div>
			{/if}
		{:else if !loading}
			<div
				class="rounded-lg border border-dashed border-zinc-700 bg-zinc-900/30 p-8 text-center text-sm text-zinc-400"
			>
				No segment efforts yet. Segments are matched automatically when you import activities with
				GPS tracks.
			</div>
		{/if}
	</section>
</div>
