<script lang="ts">
	import { onMount } from 'svelte';
	import { formatDate } from '#lib/format.js';

	type Token = {
		id: string;
		name: string;
		prefix: string;
		status: string;
		created_at: string;
		expires_at: string | null;
		last_used_at: string | null;
	};

	let tokens = $state<Token[]>([]);
	let name = $state('');
	let expiresDays = $state(0);
	let creating = $state(false);
	let error = $state<string | null>(null);
	let newToken = $state<string | null>(null);

	async function load() {
		try {
			const res = await fetch('/api/pats');
			if (res.ok) tokens = (await res.json()).tokens ?? [];
		} catch {
			/* ignore */
		}
	}
	onMount(load);

	async function create() {
		if (name.trim() === '') {
			error = 'Give the token a name.';
			return;
		}
		creating = true;
		error = null;
		newToken = null;
		try {
			const res = await fetch('/api/pats', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ name: name.trim(), expires_in_days: expiresDays })
			});
			if (!res.ok) throw new Error((await res.text()).trim());
			newToken = (await res.json()).token;
			name = '';
			await load();
		} catch (e) {
			error = (e as Error).message;
		} finally {
			creating = false;
		}
	}

	async function revoke(id: string) {
		await fetch(`/api/pats/${id}`, { method: 'DELETE' });
		await load();
	}

	const statusColor: Record<string, string> = {
		active: 'bg-emerald-500/15 text-emerald-300',
		revoked: 'bg-red-500/15 text-red-300',
		expired: 'bg-amber-500/15 text-amber-300'
	};
</script>

<section class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-5">
	<h2 class="mb-1 text-sm font-medium text-zinc-300">API tokens</h2>
	<p class="mb-3 text-xs text-zinc-500">
		Personal access tokens authenticate CLI and API clients as you. Send them as
		<code class="text-zinc-400">Authorization: Bearer cairn_pat_…</code>. The token is shown once.
	</p>

	{#if error}
		<div class="mb-3 rounded border border-red-700/50 bg-red-950/30 px-3 py-2 text-xs text-red-300">
			{error}
		</div>
	{/if}

	<div class="mb-3 flex flex-wrap items-end gap-3">
		<div class="flex-1 max-md:basis-full">
			<label for="pat-name" class="mb-1 block text-xs text-zinc-500">Name</label>
			<input
				id="pat-name"
				bind:value={name}
				placeholder="e.g. laptop CLI"
				class="w-full rounded border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-sm focus:border-accent-400 focus:outline-none"
			/>
		</div>
		<div>
			<label for="pat-expires" class="mb-1 block text-xs text-zinc-500">
				Expires (days, 0 = never)
			</label>
			<input
				id="pat-expires"
				type="number"
				min="0"
				bind:value={expiresDays}
				class="w-28 rounded border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-sm focus:border-accent-400 focus:outline-none"
			/>
		</div>
		<button
			type="button"
			disabled={creating}
			onclick={create}
			class="rounded bg-accent-500 px-3 py-1.5 text-sm font-medium text-zinc-950 hover:bg-accent-400 disabled:opacity-50"
		>
			{creating ? 'Creating…' : 'Create token'}
		</button>
	</div>

	{#if newToken}
		<div
			class="mb-3 rounded border border-amber-700/50 bg-amber-950/30 px-3 py-2 text-xs text-amber-200"
		>
			<div class="font-medium">Copy your token now — it won't be shown again:</div>
			<code class="mt-1 block break-all rounded bg-zinc-950/60 px-2 py-1 font-mono text-amber-100">
				{newToken}
			</code>
			<button
				type="button"
				onclick={() => navigator.clipboard?.writeText(newToken!)}
				class="mt-1 rounded border border-amber-700/50 px-2 py-0.5 text-amber-200 hover:bg-amber-900/30 max-md:py-2"
			>
				Copy
			</button>
		</div>
	{/if}

	{#if tokens.length > 0}
		<ul class="divide-y divide-zinc-800 rounded border border-zinc-800">
			{#each tokens as t (t.id)}
				<li class="flex items-center justify-between gap-4 px-3 py-2 text-xs max-md:flex-wrap">
					<div>
						<span class="font-medium text-zinc-200">{t.name}</span>
						<span
							class="ml-2 rounded px-1.5 py-0.5 {statusColor[t.status] ??
								'bg-zinc-800 text-zinc-400'}"
						>
							{t.status}
						</span>
						<span class="ml-2 font-mono text-zinc-500">{t.prefix}</span>
						<div class="mt-0.5 text-zinc-600">
							created {formatDate(t.created_at)}
							{#if t.last_used_at}· last used {formatDate(t.last_used_at)}{:else}· never used{/if}
							{#if t.expires_at}· expires {formatDate(t.expires_at)}{/if}
						</div>
					</div>
					{#if t.status === 'active'}
						<button
							type="button"
							onclick={() => revoke(t.id)}
							class="rounded border border-zinc-700 px-2 py-1 text-zinc-400 hover:border-red-600 hover:text-red-300 max-md:py-2"
						>
							Revoke
						</button>
					{/if}
				</li>
			{/each}
		</ul>
	{/if}
</section>
