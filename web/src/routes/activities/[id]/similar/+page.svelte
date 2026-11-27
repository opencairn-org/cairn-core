<script lang="ts">
	import { onMount } from 'svelte';
	import { formatDistance, formatDuration, formatDateOnly } from '#lib/format.js';
	import type { PageData } from './$types';

	let { data }: { data: PageData } = $props();
	const a = $derived(data.activity);

	type Row = {
		id: string;
		is_current: boolean;
		title: string;
		type: string;
		start_time: string;
		distance_m: number | null;
		moving_s: number;
		elapsed_s: number;
		avg_heart_rate: number | null;
		avg_power: number | null;
		pace_s_per_km: number | null;
		shared_segments: number;
		overlap_pct: number;
	};
	type Payload = {
		target_segments: number;
		activities: Row[];
		summary: {
			count: number;
			best_moving_s: number;
			current_moving_s: number;
			current_rank: number;
			is_best: boolean;
		};
	};

	let payload = $state<Payload | null>(null);
	let loadError = $state<string | null>(null);

	onMount(async () => {
		try {
			const res = await fetch(`/api/activities/${a.id}/similar`);
			if (!res.ok) throw new Error((await res.text()).trim());
			payload = await res.json();
		} catch (e) {
			loadError = (e as Error).message;
		}
	});

	function pace(sPerKm: number | null): string {
		if (!sPerKm) return '—';
		const m = Math.floor(sPerKm / 60);
		const s = Math.round(sPerKm % 60);
		return `${m}:${String(s).padStart(2, '0')}/km`;
	}

	// Chart geometry over the chronological group: bar per repeat, height ∝
	// moving time, current highlighted, best (fastest) green.
	const chart = $derived.by(() => {
		if (!payload || payload.activities.length < 2) return null;
		const rows = payload.activities;
		const times = rows.map((r) => r.moving_s).filter((t) => t > 0);
		if (times.length < 2) return null;
		const max = Math.max(...times);
		const min = Math.min(...times);
		const W = 760;
		const H = 160;
		const pad = 8;
		const n = rows.length;
		const bw = (W - pad * 2) / n;
		return {
			W,
			H,
			pad,
			bw,
			min,
			max,
			bars: rows.map((r, i) => {
				const frac = max > 0 ? r.moving_s / max : 0;
				const h = Math.max(2, frac * (H - pad * 2));
				return {
					x: pad + i * bw,
					y: H - pad - h,
					h,
					w: Math.max(1, bw - 2),
					row: r,
					isBest: r.moving_s === min && r.moving_s > 0
				};
			})
		};
	});
</script>

<section class="space-y-8">
	<header>
		<a href={`/activities/${a.id}`} class="text-xs text-accent-400 hover:text-accent-300">
			← {a.title || 'Activity'}
		</a>
		<h1 class="mt-2 text-2xl font-semibold tracking-tight">Similar routes</h1>
		<p class="mt-1 text-sm text-zinc-400">
			Other times you've covered roughly the same route — matched by start location, distance and
			sport.
		</p>
	</header>

	{#if loadError}
		<div class="rounded-lg border border-red-900/50 bg-red-950/30 p-4 text-sm text-red-300">
			{loadError}
		</div>
	{:else if payload && payload.summary.count > 1}
		<dl class="grid grid-cols-2 gap-4 sm:grid-cols-4">
			{#each [{ label: 'Times on this route', value: String(payload.summary.count) }, { label: 'This effort ranks', value: `#${payload.summary.current_rank} of ${payload.summary.count}` }, { label: 'Your best', value: formatDuration(payload.summary.best_moving_s) }, { label: 'This effort', value: formatDuration(payload.summary.current_moving_s) }] as c (c.label)}
				<div class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-4">
					<dt class="text-xs uppercase tracking-wide text-zinc-500">{c.label}</dt>
					<dd class="mt-1 text-xl font-semibold tabular-nums">{c.value}</dd>
				</div>
			{/each}
		</dl>

		{#if payload.summary.is_best}
			<div
				class="rounded-lg border border-emerald-800/50 bg-emerald-950/30 p-3 text-sm text-emerald-300"
			>
				🏆 This is your fastest time on this route.
			</div>
		{/if}

		{#if chart}
			<section>
				<h2 class="mb-3 text-sm font-medium uppercase tracking-wide text-zinc-400">
					Moving time per effort (oldest → newest)
				</h2>
				<svg
					viewBox={`0 0 ${chart.W} ${chart.H}`}
					class="w-full"
					role="img"
					aria-label="Moving time trend"
				>
					{#each chart.bars as b (b.row.id)}
						<rect
							x={b.x}
							y={b.y}
							width={b.w}
							height={b.h}
							rx="1.5"
							class={b.row.is_current
								? 'fill-accent-400'
								: b.isBest
									? 'fill-emerald-500'
									: 'fill-zinc-600'}
						>
							<title>{formatDateOnly(b.row.start_time)} · {formatDuration(b.row.moving_s)}</title>
						</rect>
					{/each}
				</svg>
				<div class="mt-1 flex gap-4 text-[11px] text-zinc-500">
					<span>
						<span class="inline-block h-2 w-2 rounded-sm bg-accent-400"></span> this effort
					</span>
					<span><span class="inline-block h-2 w-2 rounded-sm bg-emerald-500"></span> fastest</span>
				</div>
			</section>
		{/if}

		<section>
			<h2 class="mb-3 text-sm font-medium uppercase tracking-wide text-zinc-400">All efforts</h2>
			<div class="overflow-x-auto rounded-lg border border-zinc-800">
				<table class="w-full text-sm">
					<thead class="bg-zinc-900/60 text-xs uppercase tracking-wide text-zinc-500">
						<tr>
							<th class="px-3 py-2 text-left font-medium">Date</th>
							<th class="px-3 py-2 text-right font-medium">Distance</th>
							<th class="px-3 py-2 text-right font-medium">Moving</th>
							<th class="px-3 py-2 text-right font-medium">Pace</th>
							<th class="px-3 py-2 text-right font-medium">HR</th>
							<th class="px-3 py-2 text-right font-medium">Power</th>
							<th class="px-3 py-2 text-right font-medium">Δ best</th>
						</tr>
					</thead>
					<tbody class="divide-y divide-zinc-800">
						{#each [...payload.activities].reverse() as r (r.id)}
							<tr class={r.is_current ? 'bg-accent-500/5' : ''}>
								<td class="px-3 py-2">
									<a href={`/activities/${r.id}`} class="text-accent-400 hover:text-accent-300">
										{formatDateOnly(r.start_time)}
									</a>
									{#if r.is_current}<span class="ml-1 text-[10px] text-accent-300">
											(this)
										</span>{/if}
								</td>
								<td class="px-3 py-2 text-right tabular-nums">{formatDistance(r.distance_m)}</td>
								<td class="px-3 py-2 text-right font-medium tabular-nums">
									{formatDuration(r.moving_s)}
									{#if r.moving_s === payload.summary.best_moving_s}<span
											class="ml-1 text-emerald-400"
										>
											★
										</span>{/if}
								</td>
								<td class="px-3 py-2 text-right tabular-nums">{pace(r.pace_s_per_km)}</td>
								<td class="px-3 py-2 text-right tabular-nums">{r.avg_heart_rate ?? '—'}</td>
								<td class="px-3 py-2 text-right tabular-nums">{r.avg_power ?? '—'}</td>
								<td class="px-3 py-2 text-right tabular-nums text-zinc-400">
									{r.moving_s > payload.summary.best_moving_s
										? `+${formatDuration(r.moving_s - payload.summary.best_moving_s)}`
										: '—'}
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		</section>
	{:else if payload}
		<div
			class="rounded-lg border border-dashed border-zinc-700 bg-zinc-900/30 p-8 text-center text-sm text-zinc-400"
		>
			No similar routes found. Matches need a GPS start location and distance, and at least one
			other activity from the same start with a similar length.
		</div>
	{:else}
		<p class="text-sm text-zinc-500">Loading…</p>
	{/if}
</section>
