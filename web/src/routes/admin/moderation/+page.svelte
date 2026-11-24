<script lang="ts">
	import { formatRelativeDate } from '#lib/format.js';

	type Report = {
		id: string;
		reporter: string;
		target_kind: string;
		target_id: string;
		reason: string;
		status: string;
		created_at: string;
	};

	let reports = $state<Report[]>([]);
	let statusFilter = $state('open');
	let error = $state<string | null>(null);

	async function load() {
		error = null;
		try {
			const res = await fetch(`/api/admin/reports?status=${statusFilter}`);
			if (!res.ok) throw new Error((await res.text()).trim());
			reports = (await res.json()).reports ?? [];
		} catch (e) {
			error = (e as Error).message;
		}
	}

	async function resolve(id: string, status: string) {
		const res = await fetch(`/api/admin/reports/${id}/resolve`, {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ status })
		});
		if (res.ok) reports = reports.filter((r) => r.id !== id);
	}

	async function hideActivity(id: string) {
		await fetch(`/api/admin/activities/${id}/hide`, {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ hidden: true })
		});
		alert('Activity hidden from all non-owner views.');
	}

	$effect(() => {
		void statusFilter;
		load();
	});
</script>

<svelte:head><title>Moderation · Cairn admin</title></svelte:head>

<div class="mx-auto max-w-3xl px-4 py-6">
	<div class="mb-4 flex items-center justify-between">
		<h1 class="text-xl font-semibold">Moderation queue</h1>
		<select bind:value={statusFilter} class="rounded border px-2 py-1 text-sm">
			<option value="open">Open</option>
			<option value="reviewed">Reviewed</option>
			<option value="dismissed">Dismissed</option>
			<option value="">All</option>
		</select>
	</div>

	{#if error}<p class="rounded bg-red-50 p-3 text-sm text-red-700">{error}</p>{/if}
	{#if reports.length === 0}
		<p class="rounded-lg border border-dashed p-8 text-center text-sm text-gray-500">No reports.</p>
	{/if}

	<div class="space-y-3">
		{#each reports as r (r.id)}
			<div class="rounded-lg border bg-white p-4 shadow-sm">
				<div
					class="mb-1 flex items-center justify-between text-xs text-gray-500 max-md:flex-wrap max-md:gap-1"
				>
					<span>{r.target_kind} · {formatRelativeDate(r.created_at)} · by {r.reporter}</span>
					<span class="rounded bg-gray-100 px-2 py-0.5">{r.status}</span>
				</div>
				<p class="mb-2 text-sm">{r.reason || '(no reason given)'}</p>
				<div class="text-xs text-gray-400 max-md:break-all">target: {r.target_id}</div>
				<div class="mt-3 flex gap-2 max-md:flex-wrap">
					{#if r.target_kind === 'activity'}
						<button
							onclick={() => hideActivity(r.target_id)}
							class="rounded border px-3 py-1 text-xs text-red-600 hover:bg-red-50"
						>
							Hide activity
						</button>
					{/if}
					<button
						onclick={() => resolve(r.id, 'reviewed')}
						class="rounded border px-3 py-1 text-xs hover:bg-gray-50"
					>
						Mark reviewed
					</button>
					<button
						onclick={() => resolve(r.id, 'dismissed')}
						class="rounded border px-3 py-1 text-xs text-gray-500 hover:bg-gray-50"
					>
						Dismiss
					</button>
				</div>
			</div>
		{/each}
	</div>
</div>
