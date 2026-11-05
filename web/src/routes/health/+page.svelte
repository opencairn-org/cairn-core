<script lang="ts">
	import { onMount } from 'svelte';

	// Daily health metrics merged across providers (Garmin etc.), from /api/health.
	type Point = { day: string; value: number; unit: string; provider: string };
	type Metric = {
		type: string;
		label: string;
		format: (v: number) => string;
		series: Point[];
		latest: Point | null;
	};

	const DEFS: { type: string; label: string; format: (v: number) => string }[] = [
		{ type: 'HRV', label: 'HRV', format: (v) => `${Math.round(v)} ms` },
		{ type: 'RestingHR', label: 'Resting HR', format: (v) => `${Math.round(v)} bpm` },
		{ type: 'Sleep', label: 'Sleep', format: (v) => `${(v / 3600).toFixed(1)} h` },
		{ type: 'Weight', label: 'Weight', format: (v) => `${v.toFixed(1)} kg` },
		{ type: 'Steps', label: 'Steps', format: (v) => Math.round(v).toLocaleString() }
	];

	let metrics = $state<Metric[]>([]);
	let loaded = $state(false);
	let days = $state(90);

	async function load() {
		loaded = false;
		const out: Metric[] = [];
		for (const d of DEFS) {
			let series: Point[] = [];
			try {
				const res = await fetch(`/api/health?type=${d.type}&days=${days}`);
				if (res.ok) series = (await res.json()).series ?? [];
			} catch {
				/* ignore */
			}
			out.push({ ...d, series, latest: series.at(-1) ?? null });
		}
		metrics = out;
		loaded = true;
	}
	onMount(load);

	// Build an SVG sparkline path (normalized to 0..100 x, 0..30 y).
	function spark(series: Point[]): string {
		if (series.length < 2) return '';
		const vals = series.map((p) => p.value);
		const min = Math.min(...vals);
		const max = Math.max(...vals);
		const span = max - min || 1;
		return series
			.map((p, i) => {
				const x = (i / (series.length - 1)) * 100;
				const y = 30 - ((p.value - min) / span) * 28 - 1;
				return `${i === 0 ? 'M' : 'L'}${x.toFixed(1)},${y.toFixed(1)}`;
			})
			.join(' ');
	}

	const anyData = $derived(metrics.some((m) => m.series.length > 0));
</script>

<svelte:head><title>Health · Cairn</title></svelte:head>

<div class="mx-auto max-w-4xl px-4 py-6">
	<div class="mb-4 flex items-center justify-between">
		<h1 class="text-xl font-semibold text-zinc-100">Health</h1>
		<select
			bind:value={days}
			onchange={load}
			class="rounded border border-zinc-700 bg-zinc-950 px-2 py-1 text-xs text-zinc-300"
		>
			<option value={30}>30 days</option>
			<option value={90}>90 days</option>
			<option value={365}>1 year</option>
		</select>
	</div>

	{#if !loaded}
		<p class="text-sm text-zinc-600">Loading…</p>
	{:else if !anyData}
		<p class="text-sm text-zinc-500">
			No health data yet. Connect a provider that supplies health data and run "Import health" from
			<a href="/connections" class="text-accent-400 hover:text-accent-300">Connections</a>.
		</p>
	{:else}
		<div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
			{#each metrics as m (m.type)}
				{#if m.series.length}
					<div class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-3">
						<div class="flex items-baseline justify-between">
							<span class="text-xs uppercase tracking-wide text-zinc-500">{m.label}</span>
							{#if m.latest}<span class="text-lg font-semibold text-zinc-100"
									>{m.format(m.latest.value)}</span
								>{/if}
						</div>
						<svg viewBox="0 0 100 30" preserveAspectRatio="none" class="mt-2 h-10 w-full">
							<path
								d={spark(m.series)}
								fill="none"
								stroke="#38bdf8"
								stroke-width="1"
								vector-effect="non-scaling-stroke"
							/>
						</svg>
						<div class="mt-1 text-right text-[10px] text-zinc-600">
							{m.series.length} days{#if m.latest?.provider}
								· {m.latest.provider}{/if}
						</div>
					</div>
				{/if}
			{/each}
		</div>
	{/if}
</div>
