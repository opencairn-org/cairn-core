<script lang="ts">
	import { onMount } from 'svelte';
	import { formatDate } from '#lib/format.js';

	type Invite = {
		id: string;
		prefix: string;
		email: string;
		role: string;
		status: string;
		created_at: string;
		expires_at: string | null;
		used_at: string | null;
	};

	let invites = $state<Invite[]>([]);
	let role = $state('user');
	let email = $state('');
	let expiresDays = $state(30);
	let creating = $state(false);
	let error = $state<string | null>(null);
	let newInvite = $state<{ code: string; signup_url: string } | null>(null);

	async function load() {
		try {
			const res = await fetch('/api/admin/invites');
			if (res.ok) invites = (await res.json()).invites ?? [];
		} catch {
			/* ignore */
		}
	}
	onMount(load);

	async function create() {
		creating = true;
		error = null;
		newInvite = null;
		try {
			const res = await fetch('/api/admin/invites', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ role, email: email.trim(), expires_in_days: expiresDays })
			});
			if (!res.ok) throw new Error((await res.text()).trim());
			const body = await res.json();
			newInvite = { code: body.code, signup_url: body.signup_url };
			email = '';
			await load();
		} catch (e) {
			error = (e as Error).message;
		} finally {
			creating = false;
		}
	}

	async function revoke(id: string) {
		try {
			await fetch(`/api/admin/invites/${id}/revoke`, { method: 'POST' });
			await load();
		} catch {
			/* ignore */
		}
	}

	const statusColor: Record<string, string> = {
		active: 'bg-emerald-500/15 text-emerald-300',
		used: 'bg-zinc-700 text-zinc-300',
		expired: 'bg-amber-500/15 text-amber-300',
		revoked: 'bg-red-500/15 text-red-300'
	};
</script>

<section class="rounded-lg border border-zinc-800 bg-zinc-900/40">
	<header class="border-b border-zinc-800 px-5 py-3">
		<h2 class="text-sm font-medium uppercase tracking-wide text-zinc-400">Invites</h2>
		<p class="mt-1 text-xs text-zinc-500">
			Mint a single-use code so someone can self-register. The code is shown once — share the signup
			link with the invitee.
		</p>
	</header>

	<div class="space-y-4 p-5">
		<!-- Create -->
		<div class="flex flex-wrap items-end gap-3">
			<div>
				<label for="inv-role" class="mb-1 block text-xs text-zinc-500">Role</label>
				<select
					id="inv-role"
					bind:value={role}
					class="rounded border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-sm focus:border-accent-400 focus:outline-none"
				>
					<option value="user">user</option>
					<option value="admin">admin</option>
				</select>
			</div>
			<div>
				<label for="inv-expires" class="mb-1 block text-xs text-zinc-500">Expires in (days)</label>
				<input
					id="inv-expires"
					type="number"
					min="1"
					bind:value={expiresDays}
					class="w-24 rounded border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-sm focus:border-accent-400 focus:outline-none"
				/>
			</div>
			<div class="flex-1">
				<label for="inv-email" class="mb-1 block text-xs text-zinc-500">Pin email (optional)</label>
				<input
					id="inv-email"
					type="email"
					bind:value={email}
					placeholder="alice@example.com"
					class="w-full rounded border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-sm focus:border-accent-400 focus:outline-none"
				/>
			</div>
			<button
				type="button"
				disabled={creating}
				onclick={create}
				class="rounded bg-accent-500 px-3 py-1.5 text-sm font-medium text-zinc-950 hover:bg-accent-400 disabled:opacity-50"
			>
				{creating ? 'Creating…' : 'Create invite'}
			</button>
		</div>

		{#if error}
			<div class="rounded border border-red-700/50 bg-red-950/30 px-3 py-2 text-xs text-red-300">
				{error}
			</div>
		{/if}

		{#if newInvite}
			<div
				class="rounded border border-amber-700/50 bg-amber-950/30 px-3 py-2 text-xs text-amber-200"
			>
				<div class="font-medium">
					Invite created — copy the link now, the code won't be shown again:
				</div>
				<code
					class="mt-1 block break-all rounded bg-zinc-950/60 px-2 py-1 font-mono text-amber-100"
				>
					{newInvite.signup_url}
				</code>
				<button
					type="button"
					onclick={() => navigator.clipboard?.writeText(newInvite!.signup_url)}
					class="mt-1 rounded border border-amber-700/50 px-2 py-0.5 text-amber-200 hover:bg-amber-900/30 max-md:py-2"
				>
					Copy link
				</button>
			</div>
		{/if}

		<!-- List -->
		{#if invites.length > 0}
			<ul class="divide-y divide-zinc-800 rounded border border-zinc-800">
				{#each invites as i (i.id)}
					<li class="flex items-center justify-between gap-4 px-3 py-2 text-xs max-md:flex-wrap">
						<div>
							<span class="font-mono text-zinc-300">{i.prefix}</span>
							<span
								class="ml-2 rounded px-1.5 py-0.5 {statusColor[i.status] ??
									'bg-zinc-800 text-zinc-400'}"
							>
								{i.status}
							</span>
							<span class="ml-2 rounded bg-zinc-800 px-1.5 py-0.5 text-zinc-400">{i.role}</span>
							{#if i.email}<span class="ml-2 text-zinc-500">{i.email}</span>{/if}
							<div class="mt-0.5 text-zinc-600">
								created {formatDate(i.created_at)}{#if i.expires_at}
									· expires {formatDate(i.expires_at)}{/if}
							</div>
						</div>
						{#if i.status === 'active'}
							<button
								type="button"
								onclick={() => revoke(i.id)}
								class="rounded border border-zinc-700 px-2 py-1 text-zinc-400 hover:border-red-600 hover:text-red-300 max-md:py-2"
							>
								Revoke
							</button>
						{/if}
					</li>
				{/each}
			</ul>
		{/if}
	</div>
</section>
