<script lang="ts">
	import { onMount } from 'svelte';
	import { formatDateOnly } from '#lib/format.js';
	import type { PageData } from './$types';

	let { data }: { data: PageData } = $props();

	type Item = {
		activity_id: string;
		title: string;
		start_time: string;
		achieved_value: number;
		is_best: boolean;
	};
	type Payload = {
		metric: string;
		window_kind: string;
		window_value: number;
		best: number;
		count: number;
		items: Item[];
	};

	let payload = $state<Payload | null>(null);
	let loadError = $state<string | null>(null);

	onMount(async () => {
		try {
			const params = new URLSearchParams({
				type: data.type,
				metric: data.metric,
				window_kind: data.window,
				window_value: String(data.value)
			});
			const res = await fetch(`/api/best-efforts/history?${params}`);
			if (!res.ok) throw new Error((await res.text()).trim());
			payload = await res.json();
		} catch (e) {
			loadError = (e as Error).message;
		}
	});

	function fmtValue(v: number, metric: string): string {
		switch (metric) {
			case 'pace': {
				const m = Math.floor(v / 60);
				const s = Math.round(v % 60);
				return `${m}:${s.toString().padStart(2, '0')} /km`;
			}
			case 'speed':
				return `${(v * 3.6).toFixed(1)} km/h`;
			case 'power':
				return `${Math.round(v)} W`;
			case 'heart_rate':
				return `${Math.round(v)} bpm`;
			case 'vam':
				return `${Math.round(v)} m/h`;
		}
		return v.toFixed(0);
	}

	function fmtWindow(kind: string, value: number): string {
		if (kind === 'distance') return value >= 1000 ? `${value / 1000} km` : `${value} m`;
		if (value >= 3600) return `${Math.round(value / 3600)} h`;
		if (value >= 60) return `${Math.round(value / 60)} min`;
		return `${value} s`;
	}

	const metricLabel: Record<string, string> = {
		pace: 'Pace',
		speed: 'Speed',
		power: 'Power',
		heart_rate: 'Heart rate',
		vam: 'VAM'
	};

	const title = $derived(
		`${metricLabel[data.metric] ?? data.metric} · ${fmtWindow(data.window, data.value)}`
	);

	// Bar chart over the chronological series; taller = higher value. For pace
	// (lower is better) we still draw raw value but mark the best (min) green.
	const chart = $derived.by(() => {
		if (!payload || payload.items.length < 2) return null;
		const vals = payload.items.map((i) => i.achieved_value);
		const max = Math.max(...vals);
		const min = Math.min(...vals);
		const span = max - min || 1;
		const W = 760;
		const H = 160;
		const pad = 8;
		const bw = (W - pad * 2) / payload.items.length;
		return {
			W,
			H,
			pad,
			bw,
			bars: payload.items.map((it, i) => {
				// normalise 0..1 within [min,max]; floor at a visible height
				const norm = (it.achieved_value - min) / span;
				const h = Math.max(3, 0.15 * (H - pad * 2) + norm * 0.85 * (H - pad * 2));
				return {
					x: pad + i * bw,
					y: H - pad - h,
					h,
					w: Math.max(1, bw - 2),
					item: it
				};
			})
		};
	});
</script>

<section class="space-y-8">
	<header>
		<a href="/" class="text-xs text-accent-400 hover:text-accent-300">← Overview</a>
		<h1 class="mt-2 text-2xl font-semibold tracking-tight">Best effort · {title}</h1>
		<p class="mt-1 text-sm text-zinc-400">
			Your progression for this effort across all activities.
		</p>
	</header>

	{#if loadError}
		<div class="rounded-lg border border-red-900/50 bg-red-950/30 p-4 text-sm text-red-300">
			{loadError}
		</div>
	{:else if payload && payload.count > 0}
		<dl class="grid grid-cols-2 gap-4 sm:grid-cols-3">
			<div class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-4">
				<dt class="text-xs uppercase tracking-wide text-zinc-500">All-time best</dt>
				<dd class="mt-1 text-2xl font-semibold tabular-nums text-emerald-300">
					{fmtValue(payload.best, payload.metric)}
				</dd>
			</div>
			<div class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-4">
				<dt class="text-xs uppercase tracking-wide text-zinc-500">Activities</dt>
				<dd class="mt-1 text-2xl font-semibold tabular-nums">{payload.count}</dd>
			</div>
		</dl>

		{#if chart}
			<section>
				<h2 class="mb-3 text-sm font-medium uppercase tracking-wide text-zinc-400">
					Over time (oldest → newest)
				</h2>
				<svg
					viewBox={`0 0 ${chart.W} ${chart.H}`}
					class="w-full"
					role="img"
					aria-label="Best-effort trend"
				>
					{#each chart.bars as b (b.item.activity_id)}
						<rect
							x={b.x}
							y={b.y}
							width={b.w}
							height={b.h}
							rx="1.5"
							class={b.item.is_best ? 'fill-emerald-500' : 'fill-zinc-600'}
						>
							<title>
								{formatDateOnly(b.item.start_time)} · {fmtValue(
									b.item.achieved_value,
									payload.metric
								)}
							</title>
						</rect>
					{/each}
				</svg>
				<div class="mt-1 text-[11px] text-zinc-500">
					<span class="inline-block h-2 w-2 rounded-sm bg-emerald-500"></span> all-time best
				</div>
			</section>
		{/if}

		<section>
			<h2 class="mb-3 text-sm font-medium uppercase tracking-wide text-zinc-400">All efforts</h2>
			<div class="overflow-hidden rounded-lg border border-zinc-800">
				<table class="w-full text-sm">
					<thead class="bg-zinc-900/60 text-xs uppercase tracking-wide text-zinc-500">
						<tr>
							<th class="px-3 py-2 text-left font-medium">Date</th>
							<th class="px-3 py-2 text-left font-medium">Activity</th>
							<th class="px-3 py-2 text-right font-medium">Value</th>
						</tr>
					</thead>
					<tbody class="divide-y divide-zinc-800">
						{#each [...payload.items].reverse() as it (it.activity_id)}
							<tr class={it.is_best ? 'bg-emerald-500/5' : ''}>
								<td class="px-3 py-2 tabular-nums">{formatDateOnly(it.start_time)}</td>
								<td class="px-3 py-2">
									<a
										href={`/activities/${it.activity_id}`}
										class="text-accent-400 hover:text-accent-300"
									>
										{it.title || 'Activity'}
									</a>
								</td>
								<td class="px-3 py-2 text-right font-medium tabular-nums">
									{fmtValue(it.achieved_value, payload.metric)}
									{#if it.is_best}<span class="ml-1 text-emerald-400">★</span>{/if}
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
			No efforts recorded for this bucket.
		</div>
	{:else}
		<p class="text-sm text-zinc-500">Loading…</p>
	{/if}
</section>
