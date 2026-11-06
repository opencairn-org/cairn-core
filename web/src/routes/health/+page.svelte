<script lang="ts">
	import { onMount } from 'svelte';
	import { formatDate, formatDateOnly } from '#lib/format.js';
	import {
		formatMetricValue,
		groupMetrics,
		metricDay,
		metricLabel,
		sparkPath,
		structEntries,
		type MetricGroup,
		type StoredMetric
	} from '#lib/metrics.js';

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

	// Every other key a worker or the user stored, from the metrics store (/api/metrics).
	let groups = $state<MetricGroup[]>([]);
	let trends = $state<Record<string, number[]>>({});
	let selected = $state<string | null>(null);
	let history = $state<StoredMetric[] | null>(null);

	async function json<T>(url: string): Promise<T | null> {
		try {
			const res = await fetch(url);
			return res.ok ? ((await res.json()) as T) : null;
		} catch {
			return null;
		}
	}

	async function loadHealth(): Promise<Metric[]> {
		return Promise.all(
			DEFS.map(async (d) => {
				const series =
					(await json<{ series?: Point[] }>(`/api/health?type=${d.type}&days=${days}`))?.series ??
					[];
				return { ...d, series, latest: series.at(-1) ?? null };
			})
		);
	}

	async function loadStore(): Promise<[MetricGroup[], Record<string, number[]>]> {
		const latest = await json<{ metrics?: Record<string, StoredMetric> }>('/api/metrics/latest');
		const all = Object.values(latest?.metrics ?? {});
		const to = new Date();
		const from = new Date(to.getTime() - days * 86_400_000);
		const window = `from=${from.toISOString().slice(0, 19)}Z&to=${to.toISOString().slice(0, 19)}Z`;
		const series = await Promise.all(
			all
				.filter((m) => m.value_numeric != null)
				.map(async (m) => {
					const res = await json<{ points?: { value: number }[] }>(
						`/api/metrics/series?key=${encodeURIComponent(m.key)}&bucket=day&aggregation=last&${window}`
					);
					return [m.key, (res?.points ?? []).map((p) => p.value)] as const;
				})
		);
		return [groupMetrics(all), Object.fromEntries(series)];
	}

	async function load() {
		loaded = false;
		[metrics, [groups, trends]] = await Promise.all([loadHealth(), loadStore()]);
		loaded = true;
	}
	onMount(load);

	async function select(key: string) {
		if (selected === key) {
			selected = null;
			return;
		}
		selected = key;
		history = null;
		const res = await json<{ metrics?: StoredMetric[] }>(
			`/api/metrics?keys=${encodeURIComponent(key)}&limit=30`
		);
		if (selected === key) history = res?.metrics ?? [];
	}

	function display(m: StoredMetric): string {
		if (m.value_numeric != null) return formatMetricValue(m.key, m.value_numeric);
		return structEntries(m.value_struct)
			.map(([k, v]) => (k ? `${k}: ${v}` : v))
			.join(' · ');
	}

	function source(m: StoredMetric): string {
		const worker = m.source_worker_name
			? `${m.source_worker_name}${m.source_worker_version ? ` ${m.source_worker_version}` : ''}`
			: '';
		return [m.provider || 'manual', worker].filter(Boolean).join(' · ');
	}

	// Day values show their calendar day, other values their local time.
	function when(m: StoredMetric, time: boolean): string {
		const day = metricDay(m.ts);
		if (day) return formatDateOnly(day);
		return time ? formatDate(m.ts) : formatDateOnly(m.ts);
	}

	const anyHealth = $derived(metrics.some((m) => m.series.length > 0));
	const anyData = $derived(anyHealth || groups.length > 0);
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
		{#if anyHealth}
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
									d={sparkPath(m.series.map((p) => p.value))}
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

		{#if groups.length}
			<h2 class="mb-1 mt-8 text-sm font-semibold text-zinc-200">All metrics</h2>
			<p class="mb-4 text-xs text-zinc-500">
				Everything else your providers report, newest value per metric. Select one for its history
				and source.
			</p>
			{#each groups as g (g.namespace)}
				<h3 class="mb-2 mt-5 text-xs uppercase tracking-wide text-zinc-500">{g.title}</h3>
				<div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
					{#each g.metrics as sm (sm.key)}
						{@const trend = trends[sm.key] ?? []}
						<button
							type="button"
							onclick={() => select(sm.key)}
							aria-expanded={selected === sm.key}
							class="rounded-lg border bg-zinc-900/40 p-3 text-left transition-colors hover:border-zinc-600 {selected ===
							sm.key
								? 'border-accent-500/60'
								: 'border-zinc-800'}"
						>
							<div class="flex items-baseline justify-between gap-2">
								<span class="truncate text-xs uppercase tracking-wide text-zinc-500" title={sm.key}
									>{metricLabel(sm.key)}</span
								>
								{#if sm.value_numeric != null}
									<span class="shrink-0 text-lg font-semibold text-zinc-100">{display(sm)}</span>
								{/if}
							</div>
							{#if sm.value_numeric == null}
								<dl class="mt-2 space-y-0.5 text-xs">
									{#each structEntries(sm.value_struct) as [k, v], i (i)}
										<div class="flex justify-between gap-2">
											<dt class="truncate text-zinc-500">{k}</dt>
											<dd class="shrink-0 text-zinc-200">{v}</dd>
										</div>
									{/each}
								</dl>
							{:else if trend.length > 1}
								<svg viewBox="0 0 100 30" preserveAspectRatio="none" class="mt-2 h-10 w-full">
									<path
										d={sparkPath(trend)}
										fill="none"
										stroke="#38bdf8"
										stroke-width="1"
										vector-effect="non-scaling-stroke"
									/>
								</svg>
							{/if}
							<div class="mt-1 text-right text-[10px] text-zinc-600">
								{when(sm, false)} · {sm.provider || 'manual'}
							</div>
						</button>
					{/each}
				</div>
				{#if selected && g.metrics.some((sm) => sm.key === selected)}
					<div class="mt-3 rounded-lg border border-zinc-800 bg-zinc-950/60 p-3">
						<div class="mb-2 flex items-baseline justify-between">
							<span class="text-sm font-medium text-zinc-200"
								>{metricLabel(selected)}
								<code class="ml-1 text-[11px] text-zinc-500">{selected}</code></span
							>
							<button
								type="button"
								onclick={() => (selected = null)}
								class="text-xs text-zinc-500 hover:text-zinc-300">Close</button
							>
						</div>
						{#if history == null}
							<p class="text-xs text-zinc-600">Loading…</p>
						{:else if history.length === 0}
							<p class="text-xs text-zinc-500">No entries.</p>
						{:else}
							<div class="overflow-x-auto">
								<table class="w-full text-left text-xs">
									<thead class="text-zinc-500">
										<tr>
											<th class="py-1 pr-3 font-normal">Time</th>
											<th class="py-1 pr-3 font-normal">Value</th>
											<th class="py-1 pr-3 font-normal">Source</th>
											<th class="hidden py-1 font-normal sm:table-cell">External id</th>
										</tr>
									</thead>
									<tbody class="text-zinc-300">
										{#each history as row (row.id)}
											<tr class="border-t border-zinc-800/60 align-top">
												<td class="whitespace-nowrap py-1 pr-3">{when(row, true)}</td>
												<td class="py-1 pr-3">{display(row)}</td>
												<td class="whitespace-nowrap py-1 pr-3 text-zinc-400">{source(row)}</td>
												<td
													class="hidden break-all py-1 font-mono text-[11px] text-zinc-500 sm:table-cell"
													>{row.external_id ?? '—'}</td
												>
											</tr>
										{/each}
									</tbody>
								</table>
							</div>
						{/if}
					</div>
				{/if}
			{/each}
		{/if}
	{/if}
</div>
