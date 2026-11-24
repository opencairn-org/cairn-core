<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import ActivityMap from '#lib/components/ActivityMap.svelte';
	import SportIcon from '#lib/components/SportIcon.svelte';
	import { decodePolylineLonLat } from '#lib/polyline.js';
	import { formatDistance, formatDuration, formatDate, formatElevation } from '#lib/format.js';

	type Segment = {
		id: string;
		name: string;
		distance_m: number;
		activity_type: string;
		climb_category: string | null;
		polyline: string;
		polyline_precision: number;
		elevation_gain_m: number | null;
		avg_grade: number | null;
		max_grade: number | null;
	};
	type Effort = {
		id: string;
		activity_id: string;
		start_time: string;
		elapsed_s: number;
		moving_s: number;
		avg_heart_rate: number | null;
		avg_power: number | null;
		personal_rank: number;
		is_personal_record: boolean;
	};

	let segment = $state<Segment | null>(null);
	let efforts = $state<Effort[]>([]);
	let loading = $state(true);
	let notFound = $state(false);

	onMount(async () => {
		try {
			const res = await fetch(`/api/segments/${page.params.id}`);
			if (res.status === 404) {
				notFound = true;
			} else if (res.ok) {
				const d = await res.json();
				segment = d.segment;
				efforts = d.efforts ?? [];
			}
		} catch {
			/* ignore */
		}
		loading = false;
	});

	const coordinates = $derived(
		segment?.polyline ? decodePolylineLonLat(segment.polyline, segment.polyline_precision || 5) : []
	);

	// Efforts oldest→newest for the trend chart.
	const chrono = $derived([...efforts].sort((a, b) => a.start_time.localeCompare(b.start_time)));
	const best = $derived(efforts.reduce((m, e) => Math.min(m, e.elapsed_s), Infinity));

	// ---- inline SVG trend chart (elapsed time per effort over time) ----------
	const W = 760;
	const H = 200;
	const PAD = { t: 12, r: 12, b: 22, l: 44 };
	const plotW = W - PAD.l - PAD.r;
	const plotH = H - PAD.t - PAD.b;
	const bounds = $derived.by(() => {
		if (chrono.length === 0) return { min: 0, max: 1 };
		let min = Infinity,
			max = -Infinity;
		for (const e of chrono) {
			min = Math.min(min, e.elapsed_s);
			max = Math.max(max, e.elapsed_s);
		}
		const span = max - min || 1;
		return { min: min - span * 0.1, max: max + span * 0.1 };
	});
	function x(i: number): number {
		const n = chrono.length;
		return n <= 1 ? PAD.l + plotW / 2 : PAD.l + (i / (n - 1)) * plotW;
	}
	function y(v: number): number {
		const { min, max } = bounds;
		return PAD.t + plotH - ((v - min) / (max - min || 1)) * plotH;
	}
	const linePath = $derived(
		chrono
			.map((e, i) => `${i === 0 ? 'M' : 'L'}${x(i).toFixed(1)},${y(e.elapsed_s).toFixed(1)}`)
			.join(' ')
	);
</script>

<div class="space-y-6">
	{#if loading}
		<div class="text-sm text-zinc-500">Loading…</div>
	{:else if notFound || !segment}
		<div
			class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-8 text-center text-sm text-zinc-400"
		>
			Segment not found, or you have no efforts on it.
		</div>
	{:else}
		<header class="flex items-center gap-3">
			<span
				class="flex h-10 w-10 shrink-0 items-center justify-center rounded-full bg-zinc-800 text-accent-400"
			>
				<SportIcon type={segment.activity_type} size={20} />
			</span>
			<div>
				<h1 class="text-2xl font-semibold tracking-tight">{segment.name || 'Segment'}</h1>
				<div class="mt-0.5 flex flex-wrap gap-x-4 text-xs text-zinc-500">
					<span>{formatDistance(segment.distance_m)}</span>
					{#if segment.elevation_gain_m}<span>
							{formatElevation(segment.elevation_gain_m)} climb
						</span>{/if}
					{#if segment.avg_grade != null}<span>{segment.avg_grade.toFixed(1)}% avg</span>{/if}
					{#if segment.climb_category}<span>Cat {segment.climb_category}</span>{/if}
					<span>{efforts.length} effort{efforts.length === 1 ? '' : 's'}</span>
				</div>
			</div>
		</header>

		{#if coordinates.length > 1}
			<section class="h-72 overflow-hidden rounded-xl border border-zinc-800">
				<ActivityMap {coordinates} />
			</section>
		{/if}

		{#if chrono.length > 1}
			<section>
				<h2 class="mb-2 text-sm font-medium text-zinc-300">Time trend</h2>
				<div class="rounded-xl border border-zinc-800 bg-zinc-900/40 p-3">
					<svg viewBox={`0 0 ${W} ${H}`} class="w-full" role="img" aria-label="Effort time trend">
						{#each [0, 0.5, 1] as t (t)}
							{@const gy = PAD.t + plotH * t}
							{@const val = bounds.max - (bounds.max - bounds.min) * t}
							<line x1={PAD.l} y1={gy} x2={W - PAD.r} y2={gy} stroke="#27272a" stroke-width="1" />
							<text x={PAD.l - 6} y={gy + 3} text-anchor="end" class="fill-zinc-600 text-[10px]">
								{formatDuration(Math.round(val))}
							</text>
						{/each}
						<path d={linePath} fill="none" stroke="#38bdf8" stroke-width="1.8" />
						{#each chrono as e, i (e.id)}
							<circle
								cx={x(i)}
								cy={y(e.elapsed_s)}
								r={e.elapsed_s === best ? 4 : 2.5}
								fill={e.elapsed_s === best ? '#22c55e' : '#38bdf8'}
							>
								<title>
									{formatDate(e.start_time)}: {formatDuration(e.elapsed_s)}{e.elapsed_s === best
										? ' (PR)'
										: ''}
								</title>
							</circle>
						{/each}
					</svg>
					<p class="mt-1 text-center text-[10px] text-zinc-600">
						Each point is one effort, oldest → newest. Green = your best.
					</p>
				</div>
			</section>
		{/if}

		<section>
			<h2 class="mb-3 text-sm font-medium text-zinc-300">All efforts</h2>
			<div class="overflow-x-auto rounded-xl border border-zinc-800">
				<table class="w-full text-sm">
					<thead class="bg-zinc-900/60 text-xs uppercase tracking-wide text-zinc-500">
						<tr>
							<th class="px-4 py-2 text-left font-medium">Date</th>
							<th class="px-4 py-2 text-right font-medium">Time</th>
							<th class="px-4 py-2 text-right font-medium">Rank</th>
							<th class="px-4 py-2 text-right font-medium">HR</th>
							<th class="px-4 py-2 text-right font-medium">Power</th>
						</tr>
					</thead>
					<tbody>
						{#each efforts as e (e.id)}
							<tr class="border-t border-zinc-800/70 hover:bg-zinc-900/40">
								<td class="px-4 py-2">
									<a
										href={`/activities/${e.activity_id}`}
										class="text-accent-400 hover:text-accent-300 hover:underline"
									>
										{formatDate(e.start_time)}
									</a>
								</td>
								<td class="px-4 py-2 text-right tabular-nums">
									{formatDuration(e.elapsed_s)}
									{#if e.is_personal_record}<span
											class="ml-1 rounded bg-amber-500/15 px-1 text-[10px] text-amber-300"
										>
											PR
										</span>{/if}
								</td>
								<td class="px-4 py-2 text-right tabular-nums text-zinc-400">#{e.personal_rank}</td>
								<td class="px-4 py-2 text-right tabular-nums text-zinc-400">
									{e.avg_heart_rate ?? '—'}
								</td>
								<td class="px-4 py-2 text-right tabular-nums text-zinc-400">
									{e.avg_power ?? '—'}
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		</section>
	{/if}
</div>
