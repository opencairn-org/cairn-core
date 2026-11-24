<script lang="ts">
	import { onMount } from 'svelte';
	import SportIcon from '#lib/components/SportIcon.svelte';
	import { formatDistance, formatDuration, formatElevation } from '#lib/format.js';

	type Facet = { value: string; count: number };
	type Year = {
		year: number;
		count: number;
		distance_m: number;
		moving_s: number;
		elevation_gain_m: number;
	};
	type Record = {
		id: string;
		title: string;
		type: string;
		discipline: string;
		distance_m: number;
		elapsed_duration_s: number;
	} | null;
	type Stats = {
		totals: { count: number; distance_m: number; moving_s: number; elevation_gain_m: number };
		by_sport: Facet[];
		by_discipline: Facet[];
		by_year: Year[];
		records: { longest_distance: Record; longest_duration: Record };
	};

	let s = $state<Stats | null>(null);
	let loading = $state(true);

	onMount(async () => {
		try {
			const res = await fetch('/api/stats');
			if (res.ok) s = await res.json();
		} catch {
			/* ignore */
		}
		loading = false;
	});

	function humanize(v: string): string {
		return v.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
	}

	// max year distance for the bar widths
	const maxYearDist = $derived(s ? Math.max(1, ...s.by_year.map((y) => y.distance_m)) : 1);
	const maxSport = $derived(s ? Math.max(1, ...s.by_sport.map((f) => f.count)) : 1);
</script>

<div class="space-y-8">
	<header>
		<h1 class="text-3xl font-semibold tracking-tight max-md:text-2xl">Stats</h1>
		<p class="mt-1 text-sm text-zinc-400">
			The numbers, for the data geeks. Lifetime totals, year-by-year, and your records.
		</p>
	</header>

	{#if loading}
		<div class="text-sm text-zinc-500">Loading…</div>
	{:else if !s || s.totals.count === 0}
		<div
			class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-8 text-center text-sm text-zinc-400"
		>
			No activities yet. Import some to see your stats.
		</div>
	{:else}
		<!-- lifetime totals -->
		<div class="grid grid-cols-2 gap-4 lg:grid-cols-4">
			{#each [{ label: 'Activities', value: s.totals.count.toLocaleString() }, { label: 'Distance', value: formatDistance(s.totals.distance_m) }, { label: 'Moving time', value: formatDuration(s.totals.moving_s) }, { label: 'Elevation', value: formatElevation(s.totals.elevation_gain_m) }] as c (c.label)}
				<div class="rounded-xl border border-zinc-800 bg-zinc-900/40 p-5">
					<div class="text-xs uppercase tracking-wide text-zinc-500">{c.label}</div>
					<div class="mt-1 text-2xl font-semibold tabular-nums text-zinc-100">{c.value}</div>
				</div>
			{/each}
		</div>

		<!-- records -->
		<section>
			<h2 class="mb-3 text-sm font-medium text-zinc-300">Records</h2>
			<div class="grid gap-4 sm:grid-cols-2">
				{#each [{ label: 'Longest distance', r: s.records.longest_distance, metric: (r: NonNullable<Record>) => formatDistance(r.distance_m) }, { label: 'Longest duration', r: s.records.longest_duration, metric: (r: NonNullable<Record>) => formatDuration(r.elapsed_duration_s) }] as card (card.label)}
					<div class="rounded-xl border border-zinc-800 bg-zinc-900/40 p-5">
						<div class="text-xs uppercase tracking-wide text-zinc-500">{card.label}</div>
						{#if card.r}
							<a
								href={`/activities/${card.r.id}`}
								class="mt-2 flex items-center gap-3 hover:text-accent-300"
							>
								<span
									class="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-zinc-800 text-zinc-300"
								>
									<SportIcon type={card.r.discipline || card.r.type} size={18} />
								</span>
								<div class="min-w-0 flex-1">
									<div class="truncate text-sm font-medium">
										{card.r.title || `${humanize(card.r.discipline || card.r.type)} Activity`}
									</div>
								</div>
								<div class="text-lg font-semibold tabular-nums text-zinc-100">
									{card.metric(card.r)}
								</div>
							</a>
						{:else}
							<div class="mt-2 text-sm text-zinc-500">—</div>
						{/if}
					</div>
				{/each}
			</div>
		</section>

		<!-- by year -->
		{#if s.by_year.length > 0}
			<section>
				<h2 class="mb-3 text-sm font-medium text-zinc-300">By year</h2>
				<div class="overflow-hidden rounded-xl border border-zinc-800">
					<div class="overflow-x-auto">
						<table class="w-full text-sm">
							<thead class="bg-zinc-900/60 text-xs uppercase tracking-wide text-zinc-500">
								<tr>
									<th class="px-4 py-2 text-left font-medium">Year</th>
									<th class="px-4 py-2 text-right font-medium">Activities</th>
									<th class="px-4 py-2 text-right font-medium">Distance</th>
									<th class="px-4 py-2 text-right font-medium">Moving</th>
									<th class="px-4 py-2 text-right font-medium">Elevation</th>
									<th class="w-1/4 px-4 py-2"></th>
								</tr>
							</thead>
							<tbody>
								{#each s.by_year as y (y.year)}
									<tr class="border-t border-zinc-800/70">
										<td class="px-4 py-2 font-medium text-zinc-200">{y.year}</td>
										<td class="px-4 py-2 text-right tabular-nums text-zinc-300">{y.count}</td>
										<td class="px-4 py-2 text-right tabular-nums text-zinc-300">
											{formatDistance(y.distance_m)}
										</td>
										<td class="px-4 py-2 text-right tabular-nums text-zinc-300">
											{formatDuration(y.moving_s)}
										</td>
										<td class="px-4 py-2 text-right tabular-nums text-zinc-300">
											{formatElevation(y.elevation_gain_m)}
										</td>
										<td class="px-4 py-2">
											<div class="h-2 rounded-full bg-zinc-800">
												<div
													class="h-2 rounded-full bg-accent-500"
													style:width={`${(y.distance_m / maxYearDist) * 100}%`}
												></div>
											</div>
										</td>
									</tr>
								{/each}
							</tbody>
						</table>
					</div>
				</div>
			</section>
		{/if}

		<!-- by sport -->
		{#if s.by_sport.length > 0}
			<section>
				<h2 class="mb-3 text-sm font-medium text-zinc-300">By sport</h2>
				<div class="space-y-2">
					{#each s.by_sport as f (f.value)}
						<a
							href={`/activities?type=${f.value}`}
							class="flex items-center gap-3 rounded-lg border border-zinc-800 bg-zinc-900/40 px-4 py-2.5 hover:border-zinc-700 hover:bg-zinc-900/80"
						>
							<span class="text-accent-400"><SportIcon type={f.value} size={20} /></span>
							<span class="w-32 shrink-0 text-sm text-zinc-200 max-md:w-24">
								{humanize(f.value)}
							</span>
							<div class="h-2 flex-1 rounded-full bg-zinc-800">
								<div
									class="h-2 rounded-full bg-accent-500"
									style:width={`${(f.count / maxSport) * 100}%`}
								></div>
							</div>
							<span class="w-12 shrink-0 text-right text-sm tabular-nums text-zinc-400">
								{f.count}
							</span>
						</a>
					{/each}
				</div>
			</section>
		{/if}
	{/if}
</div>
