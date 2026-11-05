<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { m } from '#lib/paraglide/messages.js';
	import AccountsSync from '#lib/components/AccountsSync.svelte';
	import type { PageData } from './$types';

	let { data }: { data: PageData } = $props();

	type Conn = {
		id: string;
		provider: string;
		display_name: string;
		label: string;
		client_id: string;
		has_secret: boolean;
		oauth: boolean;
	};
	type Pool = {
		name: string;
		package: string;
		parser_version: string;
		release: string;
		online: boolean;
	};
	// One credential slot of the connect form, labelled by the provider's worker.
	type ConfigField = {
		key: string;
		label: string;
		help: string;
		secret: boolean;
		placeholder: string;
	};
	type ProviderType = {
		provider: string;
		display_name: string;
		oauth: boolean;
		auth_method?: string;
		setup_docs_url?: string;
		config_fields?: ConfigField[];
		worker_pools?: Pool[];
	};

	type WebhookEndpoint = { provider: string; worker_key: string; url: string };

	let connections = $state<Conn[]>([]);
	let providers = $state<ProviderType[]>([]);
	let connectedIds = $state<Set<string>>(new Set());
	let webhookEndpoints = $state<WebhookEndpoint[]>([]);
	let error = $state<string | null>(null);

	// Webhook endpoint for a given provider (first webhook-advertising worker).
	function webhookFor(provider: string): WebhookEndpoint | undefined {
		return webhookEndpoints.find((e) => e.provider === provider);
	}

	let showAdd = $state(false);
	let form = $state({
		provider: '',
		label: '',
		client_id: '',
		client_secret: '',
		worker: ''
	});

	// Several worker pools for one provider: a connection is made for one of
	// them, and the same provider account can be connected once per pool.
	function poolsFor(provider: string): Pool[] {
		return providers.find((p) => p.provider === provider)?.worker_pools ?? [];
	}
	let connectWorker = $state<Record<string, string>>({});
	function connectHref(c: Conn): string {
		const w = connectWorker[c.id];
		return `/auth/oauth/${c.provider}/connect?connection=${c.id}${w ? `&worker=${encodeURIComponent(w)}` : ''}`;
	}

	// The two credential slots are labelled by the provider's worker (a login
	// for credential providers, an app's client id/secret for OAuth ones).
	const formProvider = $derived(providers.find((p) => p.provider === form.provider));
	const formProviderOAuth = $derived(formProvider?.oauth ?? true);
	function fieldOf(provider: string, key: string): ConfigField | undefined {
		return providers
			.find((p) => p.provider === provider)
			?.config_fields?.find((f) => f.key === key);
	}
	function fieldLabel(provider: string, key: string, fallback: string): string {
		return fieldOf(provider, key)?.label || fallback;
	}
	function fieldHelp(provider: string): string {
		return (providers.find((p) => p.provider === provider)?.config_fields ?? [])
			.map((f) => f.help)
			.filter(Boolean)
			.join(' ');
	}
	let busy = $state(false);

	let editing = $state<string | null>(null);
	let editForm = $state({ label: '', client_id: '', client_secret: '' });

	const connected = $derived(page.url.searchParams.get('connected'));
	const callbackError = $derived(page.url.searchParams.get('error'));
	const errorMessages: Record<string, string> = {
		configure_first: m.connections_error_configure_first(),
		exchange_failed: m.connections_error_exchange_failed(),
		no_connection: 'Pick a connection to connect.',
		choose_worker:
			'Several importers serve this provider. Choose which one this connection is for, then connect again.',
		bad_worker: 'That importer name is not valid.',
		no_worker:
			'No importer for this provider is onboarded yet, so nobody can complete the connection.',
		already_linked:
			'This provider account is already linked to another user. Disconnect it there first.',
		secret_unreadable:
			'Stored credentials could not be read (encryption changed). Open ⚙ Settings on this connection and re-enter your client secret.'
	};

	async function load() {
		try {
			const [cRes, aRes, wRes] = await Promise.all([
				fetch('/api/connections'),
				fetch('/api/accounts'),
				fetch('/api/webhook-endpoints')
			]);
			if (cRes.ok) {
				const body = await cRes.json();
				connections = body.connections ?? [];
				providers = body.providers ?? [];
				const first = providers[0];
				if (first && !providers.find((p) => p.provider === form.provider)) {
					form.provider = first.provider;
				}
			}
			if (aRes.ok) {
				const accts = (await aRes.json()).accounts ?? [];
				connectedIds = new Set(
					accts.map((a: { connection_id: string | null }) => a.connection_id).filter(Boolean)
				);
			}
			if (wRes.ok) {
				webhookEndpoints = (await wRes.json()).endpoints ?? [];
			}
		} catch {
			/* ignore */
		}
	}

	async function addConnection() {
		busy = true;
		error = null;
		try {
			const res = await fetch('/api/connections', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(form)
			});
			if (!res.ok) throw new Error((await res.text()).trim());
			showAdd = false;
			form = { provider: form.provider, label: '', client_id: '', client_secret: '', worker: '' };
			await load();
		} catch (e) {
			error = (e as Error).message;
		} finally {
			busy = false;
		}
	}

	function startEdit(c: Conn) {
		editing = editing === c.id ? null : c.id;
		editForm = { label: c.label, client_id: c.client_id, client_secret: '' };
	}

	async function saveEdit(id: string) {
		busy = true;
		error = null;
		try {
			const res = await fetch(`/api/connections/${id}`, {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(editForm)
			});
			if (!res.ok) throw new Error((await res.text()).trim());
			editing = null;
			await load();
		} catch (e) {
			error = (e as Error).message;
		} finally {
			busy = false;
		}
	}

	async function remove(id: string) {
		if (
			!confirm(
				'Delete this connection? Imported activities stay; the connection can no longer refresh.'
			)
		)
			return;
		try {
			await fetch(`/api/connections/${id}`, { method: 'DELETE' });
			await load();
		} catch {
			/* ignore */
		}
	}

	onMount(load);
</script>

<div class="space-y-6">
	<header class="flex items-start justify-between gap-4 max-md:flex-col">
		<div>
			<h1 class="text-2xl font-semibold tracking-tight">{m.connections_title()}</h1>
			<p class="mt-1 max-w-2xl text-sm text-zinc-400">
				Each connection is a separate link to a provider with its own credentials. Add several per
				provider if you like — <strong class="text-zinc-300"
					>all import into this one Cairn account.</strong
				>
			</p>
		</div>
		<button
			type="button"
			onclick={() => (showAdd = !showAdd)}
			class="shrink-0 rounded bg-accent-500 px-3 py-2 text-sm font-medium text-zinc-950 hover:bg-accent-400"
		>
			{showAdd ? 'Cancel' : '+ Add connection'}
		</button>
	</header>

	<div class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-3 text-xs text-zinc-400">
		Connecting needs a connector (worker) for the provider onboarded on this instance.
		{#if data.isAdmin}
			<a href="/admin" class="text-accent-400 hover:text-accent-300">Onboard connectors →</a>
		{:else}
			Your platform admin manages connectors.
		{/if}
	</div>

	{#if connected}
		<div
			class="rounded border border-emerald-700/50 bg-emerald-950/30 px-3 py-2 text-sm text-emerald-300"
		>
			{m.connections_connected_ok()}
		</div>
	{/if}
	{#if callbackError}
		<div class="rounded border border-red-700/50 bg-red-950/30 px-3 py-2 text-sm text-red-300">
			{errorMessages[callbackError] ?? callbackError}
		</div>
	{/if}
	{#if error}
		<div class="rounded border border-red-700/50 bg-red-950/30 px-3 py-2 text-sm text-red-300">
			{error}
		</div>
	{/if}

	{#if showAdd}
		<div class="space-y-3 rounded-lg border border-zinc-800 bg-zinc-950/40 p-4">
			<div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
				<div>
					<label for="np" class="mb-1 block text-xs text-zinc-400">Provider</label>
					<select
						id="np"
						bind:value={form.provider}
						class="w-full rounded border border-zinc-700 bg-zinc-900 px-3 py-1.5 text-sm focus:border-accent-400 focus:outline-none"
					>
						{#each providers as p (p.provider)}
							<option value={p.provider}>{p.display_name}</option>
						{/each}
					</select>
				</div>
				<div>
					<label for="nl" class="mb-1 block text-xs text-zinc-400">Label (optional)</label>
					<input
						id="nl"
						bind:value={form.label}
						placeholder="e.g. My road bike Strava"
						class="w-full rounded border border-zinc-700 bg-zinc-900 px-3 py-1.5 text-sm focus:border-accent-400 focus:outline-none"
					/>
				</div>
				<div>
					<label for="nc" class="mb-1 block text-xs text-zinc-400">
						{fieldLabel(form.provider, 'client_id', m.connections_client_id())}
					</label>
					<input
						id="nc"
						bind:value={form.client_id}
						placeholder={fieldOf(form.provider, 'client_id')?.placeholder ?? ''}
						class="w-full rounded border border-zinc-700 bg-zinc-900 px-3 py-1.5 text-sm focus:border-accent-400 focus:outline-none"
					/>
				</div>
				<div>
					<label for="ns" class="mb-1 block text-xs text-zinc-400">
						{fieldLabel(form.provider, 'client_secret', m.connections_client_secret())}
					</label>
					<input
						id="ns"
						type="password"
						autocomplete="off"
						bind:value={form.client_secret}
						placeholder={fieldOf(form.provider, 'client_secret')?.placeholder ?? ''}
						class="w-full rounded border border-zinc-700 bg-zinc-900 px-3 py-1.5 text-sm focus:border-accent-400 focus:outline-none"
					/>
				</div>
			</div>
			{#if !formProviderOAuth && poolsFor(form.provider).length > 1}
				<div>
					<label for="nw" class="mb-1 block text-xs text-zinc-400">Importer</label>
					<select
						id="nw"
						bind:value={form.worker}
						class="w-full rounded border border-zinc-700 bg-zinc-900 px-3 py-1.5 text-sm focus:border-accent-400 focus:outline-none sm:w-1/2"
					>
						<option value="">— choose —</option>
						{#each poolsFor(form.provider) as p (p.name)}
							<option value={p.name}>{p.name} · {p.package}{p.online ? '' : ' (offline)'}</option>
						{/each}
					</select>
					<p class="mt-1 text-xs text-zinc-500">
						The same account can be connected once per importer. Leave out an importer you don't
						want.
					</p>
				</div>
			{/if}
			{#if fieldHelp(form.provider) || !formProviderOAuth || formProvider?.setup_docs_url}
				<p class="text-xs text-zinc-500">
					{fieldHelp(form.provider) ||
						(formProviderOAuth
							? ''
							: 'This provider signs in with stored credentials (encrypted). The account is linked as soon as the connection is created.')}
					{#if formProvider?.setup_docs_url}
						<a
							href={formProvider.setup_docs_url}
							target="_blank"
							rel="noreferrer"
							class="text-accent-400 hover:text-accent-300">Setup guide ↗</a
						>
					{/if}
				</p>
			{/if}
			<button
				type="button"
				disabled={busy ||
					!form.client_id.trim() ||
					(!formProviderOAuth && !form.client_secret) ||
					(!formProviderOAuth && poolsFor(form.provider).length > 1 && !form.worker)}
				onclick={addConnection}
				class="rounded bg-accent-500 px-3 py-1.5 text-sm font-medium text-zinc-950 hover:bg-accent-400 disabled:opacity-50"
			>
				{busy ? 'Saving…' : 'Create connection'}
			</button>
		</div>
	{/if}

	{#if connections.length === 0 && !showAdd}
		<p class="text-sm text-zinc-500">No connections yet. Add one to import your activities.</p>
	{/if}

	{#each connections as c (c.id)}
		<div class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-5">
			<div class="flex items-center justify-between gap-4 max-md:flex-col max-md:items-start">
				<div>
					<div class="text-base font-medium">
						{c.display_name}
						{#if c.label}<span class="ml-1 text-zinc-400">· {c.label}</span>{/if}
					</div>
					<div class="mt-0.5 text-xs text-zinc-500">
						client <span class="font-mono">{c.client_id}</span>
						{#if connectedIds.has(c.id)}<span class="ml-2 text-emerald-400">● connected</span>{/if}
					</div>
				</div>
				<div class="flex items-center gap-2 max-md:flex-wrap">
					{#if c.oauth && poolsFor(c.provider).length > 1}
						<label class="sr-only" for={`cw-${c.id}`}>Importer</label>
						<select
							id={`cw-${c.id}`}
							bind:value={connectWorker[c.id]}
							title="Which importer this connection is for"
							class="rounded border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-xs text-zinc-200"
						>
							<option value="">— importer —</option>
							{#each poolsFor(c.provider) as p (p.name)}
								<option value={p.name}>{p.name}{p.online ? '' : ' (offline)'}</option>
							{/each}
						</select>
					{/if}
					{#if c.oauth}
						<a
							href={connectHref(c)}
							class="rounded bg-accent-500 px-3 py-1.5 text-xs font-medium text-zinc-950 hover:bg-accent-400"
						>
							{connectedIds.has(c.id) ? 'Reconnect' : m.connections_connect_account()}
						</a>
					{/if}
					<button
						type="button"
						onclick={() => startEdit(c)}
						class="rounded border border-zinc-700 px-2.5 py-1.5 text-xs text-zinc-300 hover:border-accent-500 hover:text-accent-300"
					>
						⚙ Settings
					</button>
					<button
						type="button"
						onclick={() => remove(c.id)}
						class="rounded border border-zinc-700 px-2.5 py-1.5 text-xs text-zinc-400 hover:border-red-600 hover:text-red-300"
					>
						Delete
					</button>
				</div>
			</div>

			{#if editing === c.id}
				<div class="mt-3 space-y-2 rounded border border-zinc-800 bg-zinc-950/40 p-3 text-sm">
					<input
						bind:value={editForm.label}
						placeholder="Label"
						class="w-full rounded border border-zinc-700 bg-zinc-900 px-3 py-1.5 focus:border-accent-400 focus:outline-none"
					/>
					<input
						bind:value={editForm.client_id}
						placeholder={fieldLabel(c.provider, 'client_id', m.connections_client_id())}
						class="w-full rounded border border-zinc-700 bg-zinc-900 px-3 py-1.5 focus:border-accent-400 focus:outline-none"
					/>
					<input
						type="password"
						autocomplete="off"
						bind:value={editForm.client_secret}
						placeholder={c.oauth
							? m.connections_secret_placeholder()
							: `New ${fieldLabel(c.provider, 'client_secret', 'password').toLowerCase()} (empty keeps existing)`}
						class="w-full rounded border border-zinc-700 bg-zinc-900 px-3 py-1.5 focus:border-accent-400 focus:outline-none"
					/>
					<div class="flex gap-2">
						<button
							type="button"
							disabled={busy}
							onclick={() => saveEdit(c.id)}
							class="rounded bg-accent-500 px-3 py-1.5 text-xs font-medium text-zinc-950 hover:bg-accent-400 disabled:opacity-50"
						>
							Save
						</button>
						<button
							type="button"
							onclick={() => (editing = null)}
							class="px-2 py-1.5 text-xs text-zinc-500 hover:text-zinc-300"
						>
							Cancel
						</button>
					</div>
				</div>
			{/if}

			<!-- The connected account's import stats / full-sync / history live here. -->
			<div class="mt-3">
				<AccountsSync connectionId={c.id} />
			</div>

			<!-- Webhook setup — real-time imports. Shown when a webhook-capable
			     worker for this provider is online. -->
			{#if webhookFor(c.provider)}
				{@const ep = webhookFor(c.provider)!}
				<details class="mt-3 rounded border border-zinc-800 bg-zinc-950/40 px-3 py-2 text-xs">
					<summary class="cursor-pointer text-zinc-400 hover:text-zinc-200">
						Real-time imports (webhook) — optional setup
					</summary>
					<div class="mt-2 space-y-2 text-zinc-400">
						<p>
							Register this callback URL in your {c.display_name} app so new activities import the moment
							you finish them (otherwise they arrive on the next periodic sync):
						</p>
						<div class="flex items-center gap-2">
							<code class="flex-1 truncate rounded bg-zinc-900 px-2 py-1 font-mono text-zinc-200"
								>{ep.url}</code
							>
							<button
								type="button"
								onclick={() => navigator.clipboard?.writeText(ep.url)}
								class="shrink-0 rounded border border-zinc-700 px-2 py-1 text-zinc-400 hover:border-accent-500 hover:text-accent-300"
							>
								Copy
							</button>
						</div>
						<p>
							Register this URL as the webhook callback of your {c.display_name} app; the importer answers
							the provider's verification handshake itself.
						</p>
					</div>
				</details>
			{/if}
		</div>
	{/each}
</div>
