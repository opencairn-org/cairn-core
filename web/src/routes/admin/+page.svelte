<script lang="ts">
	import { m } from '#lib/paraglide/messages.js';
	import { formatDate } from '#lib/format.js';
	import WorkerOnboarding from '#lib/components/WorkerOnboarding.svelte';
	import WorkerPools from '#lib/components/WorkerPools.svelte';
	import InviteManager from '#lib/components/InviteManager.svelte';
	import type { PageData } from './$types';

	let { data }: { data: PageData } = $props();

	// Promote/demote a user (delegated administration: user → moderator → admin).
	async function setRole(id: string, role: string) {
		const res = await fetch(`/api/admin/users/${id}/role`, {
			method: 'PUT',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ role })
		});
		if (!res.ok) {
			alert('Failed to update role: ' + (await res.text()).trim());
		}
	}
</script>

<section class="space-y-6">
	<header class="flex items-baseline justify-between max-md:flex-col max-md:gap-3">
		<div>
			<h1 class="text-3xl font-semibold tracking-tight max-md:text-2xl">{m.admin_title()}</h1>
			<p class="mt-1 text-sm text-zinc-400">{m.admin_intro()}</p>
		</div>
		<div class="flex gap-2">
			<a
				href="/admin/moderation"
				class="rounded border border-zinc-700 px-3 py-1.5 text-xs text-zinc-300 hover:border-accent-500 hover:text-accent-300"
			>
				Moderation →
			</a>
			<a
				href="/admin/oidc"
				class="rounded border border-zinc-700 px-3 py-1.5 text-xs text-zinc-300 hover:border-accent-500 hover:text-accent-300"
			>
				Identity Providers →
			</a>
		</div>
	</header>

	<section class="rounded-lg border border-zinc-800 bg-zinc-900/40">
		<header class="flex items-center justify-between border-b border-zinc-800 px-5 py-3">
			<h2 class="text-sm font-medium uppercase tracking-wide text-zinc-400">{m.stat_users()}</h2>
			<span class="text-xs text-zinc-500">{data.users.length}</span>
		</header>
		<ul class="divide-y divide-zinc-800">
			{#each data.users as u (u.id)}
				<li
					class="grid grid-cols-[1fr_1fr_auto] items-center gap-5 px-5 py-3 text-sm max-md:grid-cols-1 max-md:gap-2"
				>
					<div>
						<div class="flex items-center gap-2 font-medium">
							{u.displayName || u.username}
							{#if u.role === 'admin'}
								<span
									class="rounded bg-accent-500/20 px-1.5 py-0.5 text-[10px] font-bold uppercase text-accent-300"
								>
									admin
								</span>
							{/if}
						</div>
						<div class="text-xs text-zinc-500">{u.email}</div>
					</div>
					<div class="text-xs text-zinc-500">
						<div class="font-mono max-md:break-all">{u.id}</div>
						<div class="mt-0.5">{formatDate(u.createdAt)}</div>
					</div>
					<div class="flex items-center gap-3 text-xs text-zinc-500 tabular-nums">
						<span>{u.activityCount} acts</span>
						<select
							value={u.role}
							onchange={(e) => setRole(u.id, (e.currentTarget as HTMLSelectElement).value)}
							class="rounded border border-zinc-700 bg-zinc-900 px-1.5 py-1 text-xs text-zinc-300"
							aria-label="user role"
						>
							<option value="user">user</option>
							<option value="moderator">moderator</option>
							<option value="admin">admin</option>
						</select>
					</div>
				</li>
			{:else}
				<li class="px-5 py-6 text-center text-sm text-zinc-500">{m.admin_no_users()}</li>
			{/each}
		</ul>
	</section>

	<InviteManager />

	<WorkerOnboarding />
	<WorkerPools />
</section>
