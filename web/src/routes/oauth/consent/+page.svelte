<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';

	type ScopeInfo = { scope: string; description: string; write: boolean };
	type Info = { client_name: string; client_id: string; scopes: ScopeInfo[] };

	let loading = $state(true);
	let error = $state<string | null>(null);
	let info = $state<Info | null>(null);
	let submitting = $state(false);

	// The raw OAuth request params, forwarded verbatim from /oauth/authorize.
	const q = () => page.url.searchParams;

	onMount(async () => {
		const params = q();
		const clientId = params.get('client_id') ?? '';
		const scope = params.get('scope') ?? '';
		try {
			const res = await fetch(
				`/oauth/authorize/info?client_id=${encodeURIComponent(clientId)}&scope=${encodeURIComponent(scope)}`,
				{ credentials: 'include' }
			);
			if (res.status === 401) {
				// Not signed in — bounce through login and come back here.
				const next = location.pathname + location.search;
				location.href = '/login?next=' + encodeURIComponent(next);
				return;
			}
			if (!res.ok) {
				error = (await res.text()) || 'This authorization request is invalid.';
				return;
			}
			info = (await res.json()) as Info;
		} catch (e) {
			error = (e as Error).message;
		} finally {
			loading = false;
		}
	});

	async function decide(approve: boolean) {
		submitting = true;
		error = null;
		const params = q();
		try {
			const res = await fetch('/oauth/authorize/decision', {
				method: 'POST',
				credentials: 'include',
				headers: { 'content-type': 'application/json' },
				body: JSON.stringify({
					client_id: params.get('client_id'),
					redirect_uri: params.get('redirect_uri'),
					scope: params.get('scope') ?? '',
					state: params.get('state') ?? '',
					code_challenge: params.get('code_challenge'),
					code_challenge_method: params.get('code_challenge_method') ?? 'S256',
					resource: params.get('resource') ?? '',
					approve
				})
			});
			if (!res.ok) {
				error = (await res.text()) || 'Could not complete authorization.';
				submitting = false;
				return;
			}
			const body = (await res.json()) as { redirect_uri: string };
			location.href = body.redirect_uri; // back to the client (with code or error)
		} catch (e) {
			error = (e as Error).message;
			submitting = false;
		}
	}
</script>

<div class="mx-auto flex min-h-[70vh] max-w-md flex-col justify-center">
	<div class="rounded-2xl border border-zinc-800 bg-zinc-900/50 p-6 shadow-xl">
		<div class="mb-5 flex items-center gap-3">
			<span
				class="flex h-10 w-10 items-center justify-center rounded-xl bg-gradient-to-br from-accent-400 to-accent-600 text-zinc-950"
			>
				<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none"
					><path d="M3 19h18L14.5 7l-3 5-2-3L3 19z" fill="currentColor" /></svg
				>
			</span>
			<div>
				<div class="text-xs uppercase tracking-widest text-accent-400">Authorize</div>
				<h1 class="text-lg font-semibold tracking-tight">Connect an application</h1>
			</div>
		</div>

		{#if loading}
			<p class="text-sm text-zinc-400">Loading authorization request…</p>
		{:else if error}
			<div class="rounded-lg border border-red-700/50 bg-red-950/30 px-4 py-3 text-sm text-red-300">
				{error}
			</div>
		{:else if info}
			<p class="text-sm text-zinc-300">
				<span class="font-semibold text-zinc-100">{info.client_name}</span>
				wants access to your Cairn account. It will be able to:
			</p>

			<ul class="my-4 space-y-2">
				{#each info.scopes as s (s.scope)}
					<li
						class="flex items-start gap-3 rounded-lg border border-zinc-800 bg-zinc-950/50 px-3 py-2.5"
					>
						<span
							class="mt-0.5 flex h-5 w-5 items-center justify-center rounded {s.write
								? 'bg-amber-500/15 text-amber-300'
								: 'bg-accent-500/15 text-accent-300'}"
						>
							{#if s.write}
								<svg
									class="h-3 w-3"
									viewBox="0 0 24 24"
									fill="none"
									stroke="currentColor"
									stroke-width="2.5"
									><path d="M12 20h9M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z" /></svg
								>
							{:else}
								<svg
									class="h-3 w-3"
									viewBox="0 0 24 24"
									fill="none"
									stroke="currentColor"
									stroke-width="2.5"
									><path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7z" /><circle
										cx="12"
										cy="12"
										r="2.5"
									/></svg
								>
							{/if}
						</span>
						<div>
							<div class="text-sm text-zinc-200">{s.description}</div>
							<div class="text-[11px] font-mono text-zinc-500">{s.scope}</div>
						</div>
					</li>
				{/each}
				{#if info.scopes.length === 0}
					<li class="text-sm text-zinc-400">No specific permissions requested.</li>
				{/if}
			</ul>

			<div class="mt-5 flex gap-3">
				<button
					type="button"
					onclick={() => decide(false)}
					disabled={submitting}
					class="flex-1 rounded-lg border border-zinc-700 bg-zinc-900 px-4 py-2.5 text-sm font-medium text-zinc-300 hover:border-zinc-500 disabled:opacity-50"
				>
					Deny
				</button>
				<button
					type="button"
					onclick={() => decide(true)}
					disabled={submitting}
					class="flex-1 rounded-lg bg-accent-500 px-4 py-2.5 text-sm font-semibold text-zinc-950 hover:bg-accent-400 disabled:opacity-50"
				>
					{submitting ? 'Authorizing…' : 'Allow access'}
				</button>
			</div>
			<p class="mt-4 text-center text-[11px] text-zinc-500">
				You can revoke this access any time in your account settings.
			</p>
		{/if}
	</div>
</div>
