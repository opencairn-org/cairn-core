<script lang="ts">
	import { onMount } from 'svelte';
	import { formatDate } from '#lib/format.js';
	import { enrollmentRequest, enrollmentStatus, providerError } from '#lib/enrollments.js';

	type Enrollment = {
		id: string;
		provider: string;
		name?: string;
		version?: number;
		worker_key?: string;
		worker_name_pattern?: string;
		status?: string;
		is_revoked?: boolean;
		reenroll_required?: boolean;
		expires_at?: string;
		created_at?: string;
		note?: string;
	};

	type Worker = {
		worker_name: string;
		worker_key?: string;
		instance_id: string;
		version: string;
		release?: string;
		release_compatible?: boolean;
		release_known?: boolean;
		core_release?: string;
		provider: string;
		package?: string;
		manifest_hash?: string;
		go_version?: string;
		commit?: string;
		build_date?: string;
		webhooks: boolean;
		capabilities?: Record<
			string,
			{ read?: boolean; write?: boolean; push?: boolean; backfill?: boolean; granularity?: string }
		>;
		last_seen: string;
	};

	// readableTypes lists the data types a worker can READ, in a stable order, for
	// the "this provider gives you …" display.
	function readableTypes(w: Worker): string[] {
		if (!w.capabilities) return [];
		return Object.entries(w.capabilities)
			.filter(([, c]) => c.read)
			.map(([t]) => t)
			.sort();
	}
	let workers = $state<Worker[]>([]);

	// A webhook-advertising worker owns /webhooks/{name}-{provider} (its NATS
	// worker key). The URL is shown inline next to its worker (below).
	const origin = typeof window !== 'undefined' ? window.location.origin : '';
	function workerKey(w: Worker): string {
		return `${w.worker_name}-${w.provider}`;
	}
	function webhookURL(key: string): string {
		return `${origin}/webhooks/${key}`;
	}

	let enrollments = $state<Enrollment[]>([]);
	// Built-in providers as suggestions; any well-formed provider id is accepted.
	let providers = $state<string[]>([]);
	let name = $state('');
	let provider = $state('');
	const providerHint = $derived(providerError(provider));
	let expiresDays = $state(365);
	let note = $state('');
	let creating = $state(false);
	let error = $state<string | null>(null);
	let newToken = $state<string | null>(null);

	async function load() {
		try {
			const [eRes, wRes] = await Promise.all([
				fetch('/api/admin/worker-enrollments'),
				fetch('/api/admin/workers')
			]);
			if (eRes.ok) {
				const body = await eRes.json();
				enrollments = body.enrollments ?? [];
				providers = body.providers ?? [];
			}
			if (wRes.ok) workers = (await wRes.json()).workers ?? [];
		} catch {
			/* ignore */
		}
	}

	async function create() {
		const req = enrollmentRequest(name, provider, expiresDays, note);
		if (!req) return;
		creating = true;
		error = null;
		newToken = null;
		try {
			const res = await fetch('/api/admin/worker-enrollments', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(req)
			});
			if (!res.ok) throw new Error((await res.text()).trim());
			const body = await res.json();
			newToken = body.token;
			note = '';
			name = '';
			await load();
		} catch (e) {
			error = (e as Error).message;
		} finally {
			creating = false;
		}
	}

	async function revoke(id: string) {
		try {
			await fetch(`/api/admin/worker-enrollments/${id}/revoke`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ reason: 'revoked from admin UI' })
			});
			await load();
		} catch {
			/* ignore */
		}
	}

	// Prolong: extend an enrollment's expiry (works even after it lapsed).
	async function prolong(id: string) {
		const days = Number(prompt('Extend expiry by how many days?', '365'));
		if (!Number.isFinite(days) || days <= 0) return;
		try {
			await fetch(`/api/admin/worker-enrollments/${id}/prolong`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ expires_in_hours: Math.round(days * 24) })
			});
			await load();
		} catch {
			/* ignore */
		}
	}

	function fmt(iso?: string) {
		return iso ? formatDate(iso) : '—';
	}

	// Recomputed reactively so the expiry badge stays correct over time.
	let nowMs = $state(Date.now());
	onMount(() => {
		const t = setInterval(() => (nowMs = Date.now()), 30_000);
		return () => clearInterval(t);
	});

	onMount(load);
</script>

<section class="rounded-lg border border-zinc-800 bg-zinc-900/40">
	<header class="border-b border-zinc-800 px-5 py-3">
		<h2 class="text-sm font-medium uppercase tracking-wide text-zinc-400">Worker onboarding</h2>
		<p class="mt-1 text-xs text-zinc-500">
			Connectors (workers) connect to this instance with a one-time enrollment token. Create one
			here, then set it as <code>CAIRN_WORKER_ENROLLMENT_TOKEN</code> on the worker process.
		</p>
	</header>

	<div class="space-y-4 p-5">
		<!-- Connected workers -->
		<div>
			<h3 class="mb-2 text-xs font-medium uppercase tracking-wide text-zinc-500">
				Connected workers
			</h3>
			{#if workers.length === 0}
				<p class="text-xs text-zinc-600">No workers connected right now.</p>
			{:else}
				<ul class="divide-y divide-zinc-800 rounded border border-zinc-800">
					{#each workers as w (w.instance_id)}
						<li class="px-3 py-2 text-xs">
							<div class="flex items-center justify-between gap-4">
								<div class="min-w-0">
									<span class="font-medium text-zinc-200"
										>{w.worker_key || `${w.worker_name}-${w.provider}`}</span
									>
									<span
										class="ml-2 rounded bg-accent-500/15 px-1.5 py-0.5 text-accent-300"
										title="reported provider">{w.provider}</span
									>
									<span
										class="ml-2 rounded bg-zinc-800 px-1.5 py-0.5 text-zinc-400"
										title="parser version (dataset.revision)">parser {w.version}</span
									>
									{#if w.release}
										<span
											class="ml-2 rounded px-1.5 py-0.5 {w.release_compatible
												? 'bg-zinc-800 text-zinc-400'
												: 'bg-red-500/15 text-red-300'}"
											title={w.release_compatible
												? 'worker release'
												: `Incompatible with core ${w.core_release}: major.minor must match. Ignored for re-parse and gear sync.`}
											>{w.release}{w.release_compatible ? '' : ' · incompatible'}</span
										>
									{/if}
									{#if w.webhooks}
										<span
											class="ml-2 rounded bg-sky-500/15 px-1.5 py-0.5 text-sky-300"
											title="This worker owns its webhook endpoint">webhooks</span
										>
									{/if}
									{#if w.package}
										<div class="mt-0.5 font-mono text-zinc-500" title="reported package">
											{w.package}
										</div>
									{/if}
									<div class="mt-0.5 text-zinc-600">
										{w.instance_id} · last seen {fmt(w.last_seen)}
									</div>
									{#if w.commit || w.build_date || w.go_version}
										<div
											class="mt-0.5 text-zinc-700"
											title="Build info — informational, may differ across pooled instances"
										>
											build{w.commit ? ` ${w.commit}` : ''}{w.build_date
												? ` · ${new Date(w.build_date).toLocaleString()}`
												: ''}{w.go_version ? ` · ${w.go_version}` : ''}
										</div>
									{/if}
									{#if readableTypes(w).length}
										<div
											class="mt-1 flex flex-wrap items-center gap-1"
											title="Data types this provider supplies (read/push/backfill)"
										>
											<span class="text-zinc-600">provides</span>
											{#each readableTypes(w) as t (t)}
												<span class="rounded bg-zinc-800 px-1.5 py-0.5 text-zinc-300">
													{t}{#if w.capabilities?.[t]?.push}<span
															class="ml-0.5 text-sky-400"
															title="push (webhook)">⚡</span
														>{/if}{#if w.capabilities?.[t]?.backfill}<span
															class="ml-0.5 text-zinc-500"
															title="backfill (historical)">↺</span
														>{/if}{#if w.capabilities?.[t]?.write}<span
															class="ml-0.5 text-amber-400"
															title="write-back">✎</span
														>{/if}
												</span>
											{/each}
										</div>
									{/if}
								</div>
								<span class="h-2 w-2 shrink-0 rounded-full bg-emerald-500" title="online"></span>
							</div>
							{#if w.webhooks}
								<!-- Webhook URL shown next to its owning worker -->
								<div
									class="mt-2 flex items-center gap-2 rounded border border-zinc-800 bg-zinc-950/40 px-2 py-1.5"
								>
									<span class="shrink-0 text-zinc-500">webhook</span>
									<code class="flex-1 truncate font-mono text-zinc-300"
										>{webhookURL(workerKey(w))}</code
									>
									<button
										type="button"
										onclick={() => navigator.clipboard?.writeText(webhookURL(workerKey(w)))}
										class="shrink-0 rounded border border-zinc-700 px-2 py-0.5 text-zinc-400 hover:border-accent-500 hover:text-accent-300 max-md:py-2"
									>
										Copy
									</button>
								</div>
							{/if}
						</li>
					{/each}
				</ul>
			{/if}
		</div>

		<!-- Enrollment tokens — heading-styled separator matching "Connected workers" -->
		<div class="border-t border-zinc-800 pt-4">
			<h3 class="mb-2 text-xs font-medium uppercase tracking-wide text-zinc-500">
				Enrollment tokens
			</h3>
			<p class="mb-3 text-xs text-zinc-600">
				One-time tokens that admit a new worker. The token is shown once on creation; a worker keeps
				its connection after the token expires, but can no longer reconnect or renew its credentials
				once the enrollment lapses or is revoked.
			</p>

			<!-- Create form -->
			<div class="flex flex-wrap items-end gap-3">
				<div>
					<label for="wo-name" class="mb-1 block text-xs text-zinc-500">Worker name</label>
					<input
						id="wo-name"
						bind:value={name}
						placeholder="e.g. strava-importer"
						class="w-40 rounded border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-sm focus:border-accent-400 focus:outline-none"
					/>
				</div>
				<div>
					<label for="wo-provider" class="mb-1 block text-xs text-zinc-500">Provider</label>
					<input
						id="wo-provider"
						bind:value={provider}
						list="wo-provider-suggestions"
						placeholder="e.g. strava"
						autocomplete="off"
						spellcheck="false"
						aria-invalid={providerHint !== null}
						aria-describedby="wo-provider-error"
						class="w-32 rounded border bg-zinc-900 px-2 py-1.5 text-sm focus:outline-none {providerHint
							? 'border-red-600 focus:border-red-500'
							: 'border-zinc-700 focus:border-accent-400'}"
					/>
					<datalist id="wo-provider-suggestions">
						{#each providers as p (p)}
							<option value={p}></option>
						{/each}
					</datalist>
				</div>
				<div>
					<label for="wo-expires" class="mb-1 block text-xs text-zinc-500">Expires in (days)</label>
					<input
						id="wo-expires"
						type="number"
						min="1"
						max="365"
						bind:value={expiresDays}
						class="w-28 rounded border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-sm focus:border-accent-400 focus:outline-none"
					/>
				</div>
				<div class="flex-1 max-md:basis-full">
					<label for="wo-note" class="mb-1 block text-xs text-zinc-500">Note (optional)</label>
					<input
						id="wo-note"
						bind:value={note}
						placeholder="e.g. strava worker on box-7"
						class="w-full rounded border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-sm focus:border-accent-400 focus:outline-none"
					/>
				</div>
				<button
					type="button"
					disabled={creating || !enrollmentRequest(name, provider, expiresDays, note)}
					onclick={create}
					class="rounded bg-accent-500 px-3 py-1.5 text-sm font-medium text-zinc-950 hover:bg-accent-400 disabled:opacity-50"
				>
					{creating ? 'Creating…' : 'Create enrollment'}
				</button>
			</div>

			{#if providerHint}
				<p id="wo-provider-error" class="text-xs text-red-300">{providerHint}</p>
			{/if}

			{#if error}
				<div class="rounded border border-red-700/50 bg-red-950/30 px-3 py-2 text-xs text-red-300">
					{error}
				</div>
			{/if}

			{#if newToken}
				<div
					class="rounded border border-amber-700/50 bg-amber-950/30 px-3 py-2 text-xs text-amber-200"
				>
					<div class="font-medium">Enrollment token — copy it now, it won't be shown again:</div>
					<code
						class="mt-1 block break-all rounded bg-zinc-950/60 px-2 py-1 font-mono text-amber-100"
						>{newToken}</code
					>
				</div>
			{/if}

			<!-- Existing enrollments -->
			{#if enrollments.length > 0}
				<ul class="divide-y divide-zinc-800 rounded border border-zinc-800">
					{#each enrollments as e (e.id)}
						<li class="flex items-center justify-between gap-4 px-3 py-2 text-xs max-md:flex-wrap">
							<div>
								<span class="font-medium text-zinc-200">{e.name || '—'}</span>
								{#if e.provider}
									<span class="ml-2 rounded bg-accent-500/15 px-1.5 py-0.5 text-accent-300"
										>{e.provider}</span
									>
								{/if}
								{#if enrollmentStatus(e, nowMs) === 'revoked'}
									<span class="ml-2 rounded bg-red-500/15 px-1.5 py-0.5 text-red-300">revoked</span>
								{:else if enrollmentStatus(e, nowMs) === 'reenroll'}
									<span
										class="ml-2 rounded bg-red-500/15 px-1.5 py-0.5 text-red-300"
										title="This enrollment has no provider and is refused at connect. Create a new enrollment with a provider and revoke this one."
										>re-enroll required</span
									>
								{:else if enrollmentStatus(e, nowMs) === 'expired'}
									<span class="ml-2 rounded bg-amber-500/15 px-1.5 py-0.5 text-amber-300"
										>expired</span
									>
								{:else}
									<span class="ml-2 rounded bg-emerald-500/15 px-1.5 py-0.5 text-emerald-300"
										>active</span
									>
								{/if}
								{#if e.note}<span class="ml-2 text-zinc-500">{e.note}</span>{/if}
								<div class="mt-0.5 text-zinc-600">expires {fmt(e.expires_at)} · {e.id}</div>
							</div>
							<div class="flex shrink-0 gap-2">
								{#if !e.is_revoked && !e.reenroll_required}
									<button
										type="button"
										onclick={() => prolong(e.id)}
										class="rounded border border-zinc-700 px-2 py-1 text-zinc-400 hover:border-accent-500 hover:text-accent-300 max-md:py-2"
										title="Extend the expiry (works even if expired)"
									>
										Prolong
									</button>
								{/if}
								{#if !e.is_revoked}
									<button
										type="button"
										onclick={() => revoke(e.id)}
										class="rounded border border-zinc-700 px-2 py-1 text-zinc-400 hover:border-red-600 hover:text-red-300 max-md:py-2"
									>
										Revoke
									</button>
								{/if}
							</div>
						</li>
					{/each}
				</ul>
			{/if}
		</div>
	</div>
</section>
