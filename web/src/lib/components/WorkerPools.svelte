<script lang="ts">
	import { onMount } from 'svelte';

	// The worker pool registry: a pool name is bound to one package for good.
	// Retiring hides it from routing and pickers; the same package coming
	// back un-retires it.
	type Pool = {
		provider: string;
		name: string;
		package: string;
		first_seen_at: string;
		last_seen_at: string;
		retired_at: string | null;
		online_instances: number;
	};
	let pools = $state<Pool[]>([]);
	let error = $state<string | null>(null);
	let busy = $state<string | null>(null);

	async function load() {
		try {
			const res = await fetch('/api/admin/worker-pools');
			if (res.ok) pools = (await res.json()).pools ?? [];
		} catch {
			/* ignore */
		}
	}
	onMount(load);

	async function retire(p: Pool) {
		if (
			!confirm(
				`Retire ${p.name} (${p.provider})? Accounts assigned to it keep their assignment until moved; the name stays reserved for ${p.package}.`
			)
		)
			return;
		busy = `${p.provider}/${p.name}`;
		error = null;
		try {
			const res = await fetch(`/api/admin/worker-pools/${p.provider}/${p.name}/retire`, {
				method: 'POST'
			});
			if (!res.ok) error = (await res.text()).trim() || res.statusText;
			await load();
		} finally {
			busy = null;
		}
	}
	const fmt = (s: string) => new Date(s).toLocaleString();
</script>

<section class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-5">
	<h2 class="text-sm font-medium uppercase tracking-wide text-zinc-400">Worker pools</h2>
	<p class="mt-1 text-xs text-zinc-500">
		Every worker name seen for a provider, bound to the package it reported first. Connections are
		assigned to a pool; a different implementation needs a new name.
	</p>
	{#if error}<p class="mt-2 text-xs text-red-400">{error}</p>{/if}
	{#if pools.length === 0}
		<p class="mt-3 text-xs text-zinc-600">No pools registered yet.</p>
	{:else}
		<ul class="mt-3 divide-y divide-zinc-800 rounded border border-zinc-800 text-xs">
			{#each pools as p (`${p.provider}/${p.name}`)}
				<li class="flex flex-wrap items-center gap-3 px-3 py-2">
					<span class="font-medium text-zinc-200">{p.name}</span>
					<span class="rounded bg-accent-500/15 px-1.5 py-0.5 text-accent-300">{p.provider}</span>
					<span class="font-mono text-zinc-500">{p.package}</span>
					{#if p.retired_at}
						<span class="rounded bg-zinc-800 px-1.5 py-0.5 text-zinc-400" title={fmt(p.retired_at)}
							>retired</span
						>
					{:else if p.online_instances > 0}
						<span class="text-emerald-400">● {p.online_instances} online</span>
					{:else}
						<span class="text-zinc-500">offline · last seen {fmt(p.last_seen_at)}</span>
					{/if}
					{#if !p.retired_at}
						<button
							type="button"
							disabled={busy === `${p.provider}/${p.name}`}
							onclick={() => retire(p)}
							class="ml-auto rounded border border-zinc-700 px-2 py-0.5 text-zinc-400 hover:border-red-600 hover:text-red-300 disabled:opacity-50"
						>
							Retire
						</button>
					{/if}
				</li>
			{/each}
		</ul>
	{/if}
</section>
