<script lang="ts">
	import { onMount } from 'svelte';

	// Per-activity-type provider priority editor (merge-layer brief §5). A policy
	// is a default provider-priority list plus optional per-field-group overrides;
	// this compact editor exposes the default priority (the common case) as a
	// comma-separated provider list. _any is the wildcard catch-all;
	// "provider.worker_name" names one importer pool (docs/adr/0006).

	type Policy = { default_priority: string[]; overrides?: Record<string, string[]> };

	// Activity types the editor offers rows for (the common ones; the API accepts
	// any type key).
	const TYPES = ['ride', 'run', 'swim', 'walk', 'hike', 'workout'];

	let policies = $state<Record<string, Policy>>({});
	let instanceDefaults = $state<Record<string, Policy>>({});
	let rows = $state<Record<string, string>>({}); // type -> comma-separated priority
	let loaded = $state(false);
	let saved = $state(false);
	let error = $state<string | null>(null);

	onMount(async () => {
		try {
			const res = await fetch('/api/settings/merge-policy');
			if (res.ok) {
				const b = await res.json();
				policies = b.policies ?? {};
				instanceDefaults = b.instance_defaults ?? {};
				for (const t of TYPES) {
					rows[t] = (policies[t]?.default_priority ?? []).join(', ');
				}
			}
		} catch {
			/* ignore */
		}
		loaded = true;
	});

	function placeholder(t: string): string {
		const d = instanceDefaults[t]?.default_priority;
		if (d && d.length) return d.join(', ') + ' (instance default)';
		return 'e.g. garmin, strava, _any';
	}

	async function save() {
		error = null;
		saved = false;
		const out: Record<string, Policy> = { ...policies };
		for (const t of TYPES) {
			const list = (rows[t] ?? '')
				.split(',')
				.map((s) => s.trim())
				.filter(Boolean);
			if (list.length) {
				out[t] = { ...(out[t] ?? {}), default_priority: list };
			} else {
				delete out[t];
			}
		}
		try {
			const res = await fetch('/api/settings/merge-policy', {
				method: 'PUT',
				headers: { 'content-type': 'application/json' },
				body: JSON.stringify({ policies: out })
			});
			if (!res.ok) {
				error = 'Save failed';
				return;
			}
			policies = out;
			saved = true;
			setTimeout(() => (saved = false), 2000);
		} catch {
			error = 'Save failed';
		}
	}
</script>

<section class="rounded-lg border border-zinc-800 bg-zinc-900/50 p-4">
	<h2 class="text-sm font-semibold text-zinc-200">Merge priority</h2>
	<p class="mt-1 text-xs text-zinc-500">
		When several providers report the same activity, the first provider in the list that has a value
		wins each field. Use <code class="text-zinc-400">_any</code> as a catch-all. To prefer one
		importer of a provider, name it as <code class="text-zinc-400">provider.importer</code> (for
		example
		<code class="text-zinc-400">garmin.garmin-fit</code>). Leave a row blank to use the instance
		default.
	</p>

	{#if loaded}
		<div class="mt-3 space-y-2">
			{#each TYPES as t (t)}
				<div class="flex items-center gap-3">
					<label class="w-20 shrink-0 text-xs capitalize text-zinc-400" for="mp-{t}">{t}</label>
					<input
						id="mp-{t}"
						class="flex-1 rounded border border-zinc-700 bg-zinc-950 px-2 py-1 text-xs text-zinc-200"
						bind:value={rows[t]}
						placeholder={placeholder(t)}
					/>
				</div>
			{/each}
		</div>
		<div class="mt-3 flex items-center gap-3">
			<button
				type="button"
				onclick={save}
				class="rounded bg-accent-600 px-3 py-1 text-xs font-medium text-white hover:bg-accent-500 max-md:py-2"
			>
				Save merge priority
			</button>
			{#if saved}<span class="text-xs text-emerald-400">Saved</span>{/if}
			{#if error}<span class="text-xs text-red-400">{error}</span>{/if}
		</div>
	{:else}
		<p class="mt-3 text-xs text-zinc-600">Loading…</p>
	{/if}
</section>
