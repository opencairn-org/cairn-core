<script lang="ts">
	import { onMount } from 'svelte';
	import { formatDate } from '#lib/format.js';

	type Webhook = {
		id: string;
		name: string;
		url: string;
		event_types: number[];
		min_severity: string;
		enabled: boolean;
		auto_disabled: boolean;
		last_delivery_at: string | null;
		last_status_code: number;
		last_error: string;
		consecutive_failures: number;
		created_at: string;
	};

	// Notification types — mirrors the server's notifTypeMeta order/labels.
	const typeMeta: { value: number; label: string }[] = [
		{ value: 1, label: 'Segment personal records' },
		{ value: 2, label: 'Segment course records' },
		{ value: 3, label: 'Activity imported' },
		{ value: 4, label: 'Activity updated from source' },
		{ value: 5, label: 'Connector went offline' },
		{ value: 6, label: 'Connection needs re-auth' }
	];

	let hooks = $state<Webhook[]>([]);
	let name = $state('');
	let url = $state('');
	let minSeverity = $state('info');
	let selectedTypes = $state<number[]>([]);
	let creating = $state(false);
	let error = $state<string | null>(null);
	let newSecret = $state<string | null>(null);
	let testMsg = $state<Record<string, string>>({});

	async function load() {
		try {
			const res = await fetch('/api/notifications/webhooks');
			if (res.ok) hooks = (await res.json()).webhooks ?? [];
		} catch {
			/* ignore */
		}
	}
	onMount(load);

	function toggleType(v: number) {
		selectedTypes = selectedTypes.includes(v)
			? selectedTypes.filter((t) => t !== v)
			: [...selectedTypes, v];
	}

	async function create() {
		if (name.trim() === '' || !/^https?:\/\//.test(url.trim())) {
			error = 'Give it a name and a valid http(s) URL.';
			return;
		}
		creating = true;
		error = null;
		newSecret = null;
		try {
			const res = await fetch('/api/notifications/webhooks', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({
					name: name.trim(),
					url: url.trim(),
					event_types: selectedTypes,
					min_severity: minSeverity
				})
			});
			if (!res.ok) throw new Error((await res.text()).trim());
			newSecret = (await res.json()).secret;
			name = '';
			url = '';
			selectedTypes = [];
			await load();
		} catch (e) {
			error = (e as Error).message;
		} finally {
			creating = false;
		}
	}

	async function toggleEnabled(h: Webhook) {
		await fetch(`/api/notifications/webhooks/${h.id}`, {
			method: 'PUT',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({
				name: h.name,
				url: h.url,
				event_types: h.event_types,
				min_severity: h.min_severity,
				enabled: !h.enabled && !h.auto_disabled ? true : !h.enabled
			})
		});
		await load();
	}

	async function reenable(h: Webhook) {
		await fetch(`/api/notifications/webhooks/${h.id}`, {
			method: 'PUT',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({
				name: h.name,
				url: h.url,
				event_types: h.event_types,
				min_severity: h.min_severity,
				enabled: true
			})
		});
		await load();
	}

	async function rotate(h: Webhook) {
		const res = await fetch(`/api/notifications/webhooks/${h.id}/rotate-secret`, {
			method: 'POST'
		});
		if (res.ok) newSecret = (await res.json()).secret;
	}

	async function test(h: Webhook) {
		testMsg = { ...testMsg, [h.id]: 'sending…' };
		try {
			const res = await fetch(`/api/notifications/webhooks/${h.id}/test`, { method: 'POST' });
			const body = await res.json();
			testMsg = {
				...testMsg,
				[h.id]: body.delivered
					? `delivered (${body.last_status_code})`
					: `failed: ${body.last_error || body.last_status_code || 'no response'}`
			};
		} catch (e) {
			testMsg = { ...testMsg, [h.id]: (e as Error).message };
		}
		await load();
	}

	async function remove(h: Webhook) {
		if (!confirm(`Delete webhook "${h.name}"?`)) return;
		await fetch(`/api/notifications/webhooks/${h.id}`, { method: 'DELETE' });
		await load();
	}

	function typeLabel(v: number): string {
		return typeMeta.find((t) => t.value === v)?.label ?? `type ${v}`;
	}
</script>

<section class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-5">
	<h2 class="mb-1 text-sm font-medium text-zinc-300">Outbound webhooks</h2>
	<p class="mb-3 text-xs text-zinc-500">
		POST notifications to your own services. Each delivery is signed
		<code class="text-zinc-400">X-Cairn-Signature: sha256=HMAC(secret, body)</code>. Verify it
		before trusting the payload. The signing secret is shown once.
	</p>

	{#if error}
		<div class="mb-3 rounded border border-red-700/50 bg-red-950/30 px-3 py-2 text-xs text-red-300">
			{error}
		</div>
	{/if}

	<div class="mb-3 space-y-3 rounded border border-zinc-800 bg-zinc-950/30 p-3">
		<div class="flex flex-wrap items-end gap-3">
			<div class="flex-1 max-md:basis-full">
				<label for="wh-name" class="mb-1 block text-xs text-zinc-500">Name</label>
				<input
					id="wh-name"
					bind:value={name}
					placeholder="e.g. home automation"
					class="w-full rounded border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-sm focus:border-accent-400 focus:outline-none"
				/>
			</div>
			<div class="flex-[2] max-md:basis-full">
				<label for="wh-url" class="mb-1 block text-xs text-zinc-500">URL</label>
				<input
					id="wh-url"
					bind:value={url}
					placeholder="https://example.com/cairn-hook"
					class="w-full rounded border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-sm focus:border-accent-400 focus:outline-none"
				/>
			</div>
			<div>
				<label for="wh-sev" class="mb-1 block text-xs text-zinc-500">Min severity</label>
				<select
					id="wh-sev"
					bind:value={minSeverity}
					class="rounded border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-sm focus:border-accent-400 focus:outline-none"
				>
					<option value="info">info</option>
					<option value="warn">warn</option>
					<option value="error">error</option>
				</select>
			</div>
		</div>
		<div>
			<div class="mb-1 text-xs text-zinc-500">Event types (none selected = all)</div>
			<div class="flex flex-wrap gap-1.5">
				{#each typeMeta as t (t.value)}
					<button
						type="button"
						onclick={() => toggleType(t.value)}
						class="rounded border px-2 py-0.5 text-[11px] max-md:py-1.5 {selectedTypes.includes(
							t.value
						)
							? 'border-accent-500 bg-accent-500/15 text-accent-200'
							: 'border-zinc-700 text-zinc-400 hover:border-zinc-600'}"
					>
						{t.label}
					</button>
				{/each}
			</div>
		</div>
		<button
			type="button"
			disabled={creating}
			onclick={create}
			class="rounded bg-accent-500 px-3 py-1.5 text-sm font-medium text-zinc-950 hover:bg-accent-400 disabled:opacity-50"
		>
			{creating ? 'Creating…' : 'Add webhook'}
		</button>
	</div>

	{#if newSecret}
		<div
			class="mb-3 rounded border border-amber-700/50 bg-amber-950/30 px-3 py-2 text-xs text-amber-200"
		>
			<div class="font-medium">Copy the signing secret now — it won't be shown again:</div>
			<code class="mt-1 block break-all rounded bg-zinc-950/60 px-2 py-1 font-mono text-amber-100">
				{newSecret}
			</code>
			<button
				type="button"
				onclick={() => navigator.clipboard?.writeText(newSecret!)}
				class="mt-1 rounded border border-amber-700/50 px-2 py-0.5 text-amber-200 hover:bg-amber-900/30 max-md:py-2"
			>
				Copy
			</button>
		</div>
	{/if}

	{#if hooks.length > 0}
		<ul class="divide-y divide-zinc-800 rounded border border-zinc-800">
			{#each hooks as h (h.id)}
				<li class="px-3 py-2.5 text-xs">
					<div class="flex items-start justify-between gap-3 max-md:flex-wrap">
						<div class="min-w-0">
							<span class="font-medium text-zinc-200">{h.name}</span>
							{#if h.auto_disabled}
								<span class="ml-2 rounded bg-red-500/15 px-1.5 py-0.5 text-red-300">
									auto-disabled
								</span>
							{:else if !h.enabled}
								<span class="ml-2 rounded bg-zinc-700/40 px-1.5 py-0.5 text-zinc-400">paused</span>
							{:else}
								<span class="ml-2 rounded bg-emerald-500/15 px-1.5 py-0.5 text-emerald-300">
									active
								</span>
							{/if}
							<div class="mt-0.5 truncate font-mono text-zinc-500">{h.url}</div>
							<div class="mt-0.5 text-zinc-600">
								{h.event_types.length === 0
									? 'all events'
									: h.event_types.map(typeLabel).join(', ')}
								· ≥ {h.min_severity}
							</div>
							{#if h.last_delivery_at}
								<div class="mt-0.5 text-zinc-600">
									last {formatDate(h.last_delivery_at)} · {h.last_error
										? `error: ${h.last_error}`
										: `HTTP ${h.last_status_code}`}
									{#if h.consecutive_failures > 0}· {h.consecutive_failures} consecutive failures{/if}
								</div>
							{/if}
							{#if testMsg[h.id]}
								<div class="mt-0.5 text-accent-300">test: {testMsg[h.id]}</div>
							{/if}
						</div>
						<div
							class="flex shrink-0 flex-wrap items-center justify-end gap-2 max-md:justify-start"
						>
							<button
								type="button"
								onclick={() => test(h)}
								class="rounded border border-zinc-700 px-2 py-1 text-zinc-400 hover:border-accent-500 hover:text-accent-300 max-md:py-2"
							>
								Test
							</button>
							<button
								type="button"
								onclick={() => rotate(h)}
								class="rounded border border-zinc-700 px-2 py-1 text-zinc-400 hover:border-accent-500 hover:text-accent-300 max-md:py-2"
							>
								Rotate secret
							</button>
							{#if h.auto_disabled}
								<button
									type="button"
									onclick={() => reenable(h)}
									class="rounded border border-zinc-700 px-2 py-1 text-zinc-400 hover:border-emerald-600 hover:text-emerald-300 max-md:py-2"
								>
									Re-enable
								</button>
							{:else}
								<button
									type="button"
									onclick={() => toggleEnabled(h)}
									class="rounded border border-zinc-700 px-2 py-1 text-zinc-400 hover:border-zinc-500 max-md:py-2"
								>
									{h.enabled ? 'Pause' : 'Resume'}
								</button>
							{/if}
							<button
								type="button"
								onclick={() => remove(h)}
								class="rounded border border-zinc-700 px-2 py-1 text-zinc-400 hover:border-red-600 hover:text-red-300 max-md:py-2"
							>
								Delete
							</button>
						</div>
					</div>
				</li>
			{/each}
		</ul>
	{/if}
</section>
