<script lang="ts">
	import { onMount } from 'svelte';

	// The provider registry: what each worker package declared about
	// connecting its provider, bound to the first package that published it.
	// Unbinding lets another package take the provider over; the live
	// manifests are re-applied at once.
	type Provider = {
		provider: string;
		package: string;
		display_name: string;
		auth_method: string;
		oauth: boolean;
		setup_docs_url: string;
		updated_at: string;
	};
	let providers = $state<Provider[]>([]);
	let error = $state<string | null>(null);
	let busy = $state<string | null>(null);

	async function load() {
		try {
			const res = await fetch('/api/admin/providers');
			if (res.ok) providers = (await res.json()).providers ?? [];
		} catch {
			/* ignore */
		}
	}
	onMount(load);

	async function unbind(p: Provider) {
		if (
			!confirm(
				`Unbind ${p.display_name || p.provider}? The next manifest from any package declares it again; stop or retire the pools of ${p.package || 'the old package'} first, or they rebind it.`
			)
		)
			return;
		busy = p.provider;
		error = null;
		try {
			const res = await fetch(`/api/admin/providers/${p.provider}`, { method: 'DELETE' });
			if (!res.ok) error = (await res.text()).trim() || res.statusText;
			await load();
		} finally {
			busy = null;
		}
	}
	const fmt = (s: string) => new Date(s).toLocaleString();
</script>

<section class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-5">
	<h2 class="text-sm font-medium uppercase tracking-wide text-zinc-400">Providers</h2>
	<p class="mt-1 text-xs text-zinc-500">
		How each provider is connected, as its worker declared it, bound to the package that declared it
		first. Unbind a provider to let a different package take it over.
	</p>
	{#if error}<p class="mt-2 text-xs text-red-400">{error}</p>{/if}
	{#if providers.length === 0}
		<p class="mt-3 text-xs text-zinc-600">No provider declared yet.</p>
	{:else}
		<ul class="mt-3 divide-y divide-zinc-800 rounded border border-zinc-800 text-xs">
			{#each providers as p (p.provider)}
				<li class="flex flex-wrap items-center gap-3 px-3 py-2">
					<span class="font-medium text-zinc-200">{p.display_name || p.provider}</span>
					<span class="rounded bg-accent-500/15 px-1.5 py-0.5 text-accent-300">{p.provider}</span>
					{#if p.package}
						<span class="font-mono text-zinc-500">{p.package}</span>
					{:else}
						<span class="text-zinc-500">seeded, not bound yet</span>
					{/if}
					<span class="text-zinc-500">{p.auth_method}</span>
					<span class="text-zinc-600" title={fmt(p.updated_at)}>updated {fmt(p.updated_at)}</span>
					<button
						type="button"
						disabled={busy === p.provider}
						onclick={() => unbind(p)}
						class="ml-auto rounded border border-zinc-700 px-2 py-0.5 text-zinc-400 hover:border-red-600 hover:text-red-300 disabled:opacity-50"
					>
						Unbind
					</button>
				</li>
			{/each}
		</ul>
	{/if}
</section>
