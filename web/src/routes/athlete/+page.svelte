<script lang="ts">
	import { onMount } from 'svelte';
	import { formatDateOnly } from '#lib/format.js';

	type Spec = { key: string; label: string; unit: string; min: number; max: number };
	type Entry = { id: string; key: string; effective_date: string; value: number };

	let specs = $state<Spec[]>([]);
	let entries = $state<Entry[]>([]);
	let loading = $state(true);
	let error = $state<string | null>(null);

	// per-metric "add entry" draft, keyed by metric key
	let draft = $state<Record<string, { date: string; value: string }>>({});
	let recalcBusy = $state(false);
	let recalcMsg = $state<string | null>(null);

	function today(): string {
		return new Date().toISOString().slice(0, 10);
	}

	// What each metric is and why we need it — shown under the metric heading so
	// athletes understand exactly how their input affects calculations.
	const WHY: Record<string, { what: string; used: string }> = {
		ftp_watts: {
			what: 'Functional Threshold Power — the highest power (watts) you can hold for ~1 hour.',
			used: 'The reference for power-based TSS on rides: Intensity = NormalizedPower ÷ FTP. It’s the single most important value for cycling load — get this right first.'
		},
		threshold_hr: {
			what: 'Lactate-threshold heart rate — the HR you can sustain at roughly a 1-hour all-out effort.',
			used: 'The reference for heart-rate-based TSS, used for runs and any activity without power: Intensity = avgHR ÷ threshold HR.'
		},
		max_hr: {
			what: 'Your highest observed heart rate.',
			used: 'Bounds and sanity-checks HR-based intensity, and anchors heart-rate training zones. Not used directly for TSS yet, but informs zone displays.'
		},
		resting_hr: {
			what: 'Your heart rate at complete rest (e.g. on waking).',
			used: 'With max HR it defines heart-rate reserve (Karvonen), a more personalised intensity scale, and is a simple recovery/fitness trend marker.'
		},
		weight_kg: {
			what: 'Body weight.',
			used: 'Turns power into power-to-weight (W/kg) — the number that actually predicts climbing/road performance — and improves calorie and running-power estimates. It drifts, so dating it keeps old activities accurate.'
		},
		height_cm: {
			what: 'Body height.',
			used: 'Feeds BMI and physiological estimates (e.g. running economy, body-surface models for heat/calorie math). Rarely changes, but kept here so the profile is complete.'
		},
		threshold_pace: {
			what: 'Your threshold running pace — sustainable pace for ~1 hour (entered as seconds per km).',
			used: 'The reference for pace-based running TSS (rTSS), the run equivalent of FTP for sports where you track speed rather than power.'
		}
	};

	async function load() {
		loading = true;
		try {
			const res = await fetch('/api/athlete/metrics');
			if (res.ok) {
				const d = await res.json();
				specs = d.specs ?? [];
				entries = d.entries ?? [];
				for (const s of specs) if (!draft[s.key]) draft[s.key] = { date: today(), value: '' };
			}
		} catch {
			/* ignore */
		}
		loading = false;
	}

	onMount(load);

	const byKey = $derived.by(() => {
		const m: Record<string, Entry[]> = {};
		for (const e of entries) (m[e.key] ??= []).push(e);
		for (const list of Object.values(m))
			list.sort((a, b) => b.effective_date.localeCompare(a.effective_date));
		return m;
	});

	async function add(spec: Spec) {
		error = null;
		const d = draft[spec.key];
		const value = parseFloat(d?.value ?? '');
		if (!d?.date || isNaN(value)) {
			error = `Enter a date and a ${spec.label.toLowerCase()} value.`;
			return;
		}
		try {
			const res = await fetch('/api/athlete/metrics', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ key: spec.key, effective_date: d.date, value })
			});
			if (!res.ok) throw new Error((await res.text()).trim());
			draft[spec.key] = { date: today(), value: '' };
			await load();
		} catch (e) {
			error = (e as Error).message;
		}
	}

	async function remove(id: string) {
		try {
			await fetch(`/api/athlete/metrics/${id}`, { method: 'DELETE' });
			await load();
		} catch {
			/* ignore */
		}
	}

	async function recalculate() {
		recalcBusy = true;
		recalcMsg = null;
		try {
			const res = await fetch('/api/athlete/recalculate', { method: 'POST' });
			if (!res.ok) throw new Error((await res.text()).trim());
			const d = await res.json();
			recalcMsg = `Recalculated ${d.recomputed} activities · ${d.training_load_days} training-load days updated.`;
		} catch (e) {
			recalcMsg = `Failed: ${(e as Error).message}`;
		} finally {
			recalcBusy = false;
		}
	}

	function fmtValue(spec: Spec, v: number): string {
		// Threshold pace is stored as sec/km — render as m:ss /km.
		if (spec.key === 'threshold_pace') {
			const m = Math.floor(v / 60);
			const s = Math.round(v % 60);
			return `${m}:${s.toString().padStart(2, '0')} /km`;
		}
		return `${v % 1 === 0 ? v : v.toFixed(1)} ${spec.unit}`;
	}
</script>

<div class="space-y-6">
	<header>
		<h1 class="text-3xl font-semibold tracking-tight max-md:text-2xl">Athlete profile</h1>
		<p class="mt-1 max-w-2xl text-sm text-zinc-400">
			Your real physiological values, used to calculate Training Stress Score (TSS) and the fitness
			curves. Each value is <b>time-based</b> — add a new entry whenever it changes (e.g. a new FTP test,
			a weight change). Calculations interpolate between the nearest dates, so a re-computed old activity
			uses the values that were true back then.
		</p>
	</header>

	<div
		class="flex flex-wrap items-center gap-3 rounded-xl border border-zinc-800 bg-zinc-900/40 p-4"
	>
		<div class="text-sm text-zinc-300">
			Changed your values? Recompute TSS + the training-load history with the new numbers.
		</div>
		<button
			type="button"
			onclick={recalculate}
			disabled={recalcBusy}
			class="rounded bg-accent-500 px-3 py-1.5 text-sm font-medium text-zinc-950 hover:bg-accent-400 disabled:opacity-50"
		>
			{recalcBusy ? 'Recalculating…' : 'Recalculate training load'}
		</button>
		{#if recalcMsg}<span class="text-xs text-zinc-400">{recalcMsg}</span>{/if}
	</div>

	{#if error}
		<div class="rounded border border-red-700/50 bg-red-950/30 px-3 py-2 text-xs text-red-300">
			{error}
		</div>
	{/if}

	{#if loading}
		<div class="text-sm text-zinc-500">Loading…</div>
	{:else}
		<div class="grid gap-4 lg:grid-cols-2">
			{#each specs as spec (spec.key)}
				{@const why = WHY[spec.key]}
				{@const dr = draft[spec.key]}
				<section class="rounded-xl border border-zinc-800 bg-zinc-900/40 p-4">
					<div class="mb-1 flex items-baseline justify-between">
						<h2 class="text-sm font-medium text-zinc-200">{spec.label}</h2>
						<span class="text-xs text-zinc-500">{spec.unit}</span>
					</div>
					{#if why}
						<p class="mb-3 text-xs leading-relaxed text-zinc-500">
							<span class="text-zinc-400">{why.what}</span>
							{why.used}
						</p>
					{/if}

					{#if (byKey[spec.key] ?? []).length === 0}
						<p class="mb-3 text-xs text-zinc-600">No entries yet.</p>
					{:else}
						<ul class="mb-3 divide-y divide-zinc-800 rounded border border-zinc-800">
							{#each byKey[spec.key] ?? [] as e (e.id)}
								<li class="flex items-center justify-between gap-3 px-3 py-1.5 text-sm">
									<span class="text-zinc-400">{formatDateOnly(e.effective_date)}</span>
									<span class="flex items-center gap-3">
										<span class="font-medium tabular-nums text-zinc-100">
											{fmtValue(spec, e.value)}
										</span>
										<button
											type="button"
											onclick={() => remove(e.id)}
											class="text-zinc-600 hover:text-red-400"
											aria-label="Delete"
											title="Delete"
										>
											✕
										</button>
									</span>
								</li>
							{/each}
						</ul>
					{/if}

					{#if dr}
						<div class="flex items-end gap-2 max-md:flex-wrap">
							<div>
								<label
									for={`d-${spec.key}`}
									class="mb-1 block text-[10px] uppercase tracking-wide text-zinc-500"
								>
									Date
								</label>
								<input
									id={`d-${spec.key}`}
									type="date"
									bind:value={dr.date}
									class="rounded border border-zinc-700 bg-zinc-900 px-2 py-1 text-sm focus:border-accent-400 focus:outline-none"
								/>
							</div>
							<div>
								<label
									for={`v-${spec.key}`}
									class="mb-1 block text-[10px] uppercase tracking-wide text-zinc-500"
								>
									Value ({spec.unit})
								</label>
								<input
									id={`v-${spec.key}`}
									type="number"
									step="any"
									min={spec.min}
									max={spec.max}
									placeholder={`${spec.min}–${spec.max}`}
									bind:value={dr.value}
									class="w-28 rounded border border-zinc-700 bg-zinc-900 px-2 py-1 text-sm focus:border-accent-400 focus:outline-none"
								/>
							</div>
							<button
								type="button"
								onclick={() => add(spec)}
								class="rounded border border-accent-500 bg-accent-500/20 px-3 py-1 text-sm text-accent-300 hover:bg-accent-500/30"
							>
								Add
							</button>
						</div>
					{/if}
				</section>
			{/each}
		</div>
	{/if}
</div>
