<script lang="ts">
	import { onMount } from 'svelte';
	import { formatDate } from '#lib/format.js';

	type App = {
		client_id: string;
		client_name: string;
		software_id: string;
		software_version: string;
		scopes: string[];
		active_tokens: number;
		granted_at: string;
		last_issued_at: string;
	};

	let apps = $state<App[]>([]);
	let error = $state<string | null>(null);
	let loaded = $state(false);

	async function load() {
		try {
			const res = await fetch('/api/oauth/apps');
			if (res.ok) apps = (await res.json()).apps ?? [];
		} catch {
			/* ignore */
		} finally {
			loaded = true;
		}
	}
	onMount(load);

	async function revoke(a: App) {
		if (!confirm(`Revoke access for "${a.client_name}"? The app will have to sign in again.`))
			return;
		error = null;
		try {
			const res = await fetch(`/api/oauth/apps/${encodeURIComponent(a.client_id)}`, {
				method: 'DELETE'
			});
			if (!res.ok) throw new Error((await res.text()).trim());
			await load();
		} catch (e) {
			error = (e as Error).message;
		}
	}
</script>

<section class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-5">
	<h2 class="mb-1 text-sm font-medium text-zinc-300">Authorized apps</h2>
	<p class="mb-3 text-xs text-zinc-500">
		Apps you signed into with your Cairn account (mobile clients, integrations). Revoking ends every
		token the app holds.
	</p>

	{#if error}
		<div class="mb-3 rounded border border-red-700/50 bg-red-950/30 px-3 py-2 text-xs text-red-300">
			{error}
		</div>
	{/if}

	{#if loaded && apps.length === 0}
		<p class="text-xs text-zinc-600">No apps are authorized.</p>
	{:else if apps.length > 0}
		<ul class="divide-y divide-zinc-800 rounded border border-zinc-800">
			{#each apps as a (a.client_id)}
				<li class="flex items-center justify-between gap-4 px-3 py-2 text-xs max-md:flex-wrap">
					<div>
						<span class="font-medium text-zinc-200">{a.client_name}</span>
						{#if a.software_id}
							<span class="ml-2 text-zinc-500">{a.software_id} {a.software_version}</span>
						{/if}
						<div class="mt-0.5 text-zinc-600">
							{a.scopes.join(', ') || 'no scopes'} · granted {formatDate(a.granted_at)} · {a.active_tokens}
							active {a.active_tokens === 1 ? 'token' : 'tokens'}
						</div>
					</div>
					<button
						type="button"
						onclick={() => revoke(a)}
						class="rounded border border-zinc-700 px-2 py-1 text-zinc-400 hover:border-red-700 hover:text-red-300 max-md:py-2"
					>
						Revoke
					</button>
				</li>
			{/each}
		</ul>
	{/if}
</section>
