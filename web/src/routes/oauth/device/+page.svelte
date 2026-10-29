<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';

	// RFC 8628 verification page: the person types (or arrives with) the code
	// another device shows, sees who asks for what, and approves or denies.
	type ScopeInfo = { scope: string; description: string; write: boolean };
	type Info = { client_name: string; client_id: string; scopes: ScopeInfo[]; expires_at: string };

	let code = $state(page.url.searchParams.get('user_code') ?? '');
	let info = $state<Info | null>(null);
	let error = $state<string | null>(null);
	let busy = $state(false);
	let done = $state<'approved' | 'denied' | null>(null);

	function formatCode(raw: string): string {
		const digits = raw.replace(/\D/g, '').slice(0, 8);
		return digits.length > 4 ? digits.slice(0, 4) + '-' + digits.slice(4) : digits;
	}

	async function lookup() {
		if (code.replace(/\D/g, '').length !== 8) return;
		busy = true;
		error = null;
		try {
			const res = await fetch(`/oauth/device/info?user_code=${encodeURIComponent(code)}`, {
				credentials: 'include'
			});
			if (res.status === 401) {
				const next = location.pathname + '?user_code=' + encodeURIComponent(code);
				location.href = '/login?next=' + encodeURIComponent(next);
				return;
			}
			if (!res.ok) {
				error = (await res.text()) || 'No device is waiting for this code.';
				info = null;
				return;
			}
			info = (await res.json()) as Info;
		} catch (e) {
			error = (e as Error).message;
		} finally {
			busy = false;
		}
	}

	async function decide(approve: boolean) {
		busy = true;
		error = null;
		try {
			const res = await fetch('/oauth/device/decision', {
				method: 'POST',
				credentials: 'include',
				headers: { 'content-type': 'application/json' },
				body: JSON.stringify({ user_code: code, approve })
			});
			if (!res.ok) {
				error = (await res.text()) || 'Could not record the decision.';
				return;
			}
			done = approve ? 'approved' : 'denied';
		} catch (e) {
			error = (e as Error).message;
		} finally {
			busy = false;
		}
	}

	onMount(() => {
		if (code) void lookup();
	});
</script>

<div class="mx-auto flex min-h-[70vh] max-w-md flex-col justify-center">
	<div class="rounded-2xl border border-zinc-800 bg-zinc-900/50 p-6 shadow-xl">
		<div class="mb-5 flex items-center gap-3">
			<span
				class="flex h-10 w-10 items-center justify-center rounded-xl bg-gradient-to-br from-accent-400 to-accent-600 text-zinc-950"
			>
				<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
					><rect x="3" y="4" width="18" height="12" rx="2" /><path d="M8 20h8M12 16v4" /></svg
				>
			</span>
			<div>
				<div class="text-xs uppercase tracking-widest text-accent-400">Sign in a device</div>
				<h1 class="text-lg font-semibold tracking-tight">Enter the code shown on your device</h1>
			</div>
		</div>

		{#if done}
			<p class="text-sm text-zinc-300">
				{#if done === 'approved'}
					The device is signed in. You can close this page and go back to it.
				{:else}
					The request was denied. The device will not get access.
				{/if}
			</p>
		{:else}
			<form
				class="flex gap-2"
				onsubmit={(e) => {
					e.preventDefault();
					void lookup();
				}}
			>
				<input
					class="flex-1 rounded-lg border border-zinc-700 bg-zinc-950 px-3 py-2.5 text-center font-mono text-xl tracking-[0.3em] text-zinc-100 focus:border-accent-500 focus:outline-none"
					inputmode="numeric"
					autocomplete="one-time-code"
					placeholder="0000-0000"
					bind:value={code}
					oninput={() => (code = formatCode(code))}
					disabled={busy}
				/>
				<button
					type="submit"
					disabled={busy || code.replace(/\D/g, '').length !== 8}
					class="rounded-lg bg-accent-500 px-4 py-2.5 text-sm font-semibold text-zinc-950 hover:bg-accent-400 disabled:opacity-50"
				>
					Next
				</button>
			</form>

			{#if error}
				<div
					class="mt-4 rounded-lg border border-red-700/50 bg-red-950/30 px-4 py-3 text-sm text-red-300"
				>
					{error}
				</div>
			{/if}

			{#if info}
				<p class="mt-5 text-sm text-zinc-300">
					<span class="font-semibold text-zinc-100">{info.client_name}</span>
					wants to sign in to your Cairn account. It will be able to:
				</p>
				<ul class="my-4 space-y-2">
					{#each info.scopes as s (s.scope)}
						<li
							class="flex items-start gap-3 rounded-lg border border-zinc-800 bg-zinc-950/50 px-3 py-2.5"
						>
							<span
								class="mt-0.5 flex h-5 w-5 items-center justify-center rounded text-[10px] font-bold {s.write
									? 'bg-amber-500/15 text-amber-300'
									: 'bg-accent-500/15 text-accent-300'}"
							>
								{s.write ? 'W' : 'R'}
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
				<p class="text-xs text-zinc-500">
					Only approve a code you read from a device in front of you. Someone who sends you a code
					is trying to get into your account.
				</p>
				<div class="mt-5 flex gap-3">
					<button
						type="button"
						onclick={() => decide(false)}
						disabled={busy}
						class="flex-1 rounded-lg border border-zinc-700 bg-zinc-900 px-4 py-2.5 text-sm font-medium text-zinc-300 hover:border-zinc-500 disabled:opacity-50"
					>
						Deny
					</button>
					<button
						type="button"
						onclick={() => decide(true)}
						disabled={busy}
						class="flex-1 rounded-lg bg-accent-500 px-4 py-2.5 text-sm font-semibold text-zinc-950 hover:bg-accent-400 disabled:opacity-50"
					>
						{busy ? 'Working…' : 'Sign in this device'}
					</button>
				</div>
			{/if}
		{/if}
	</div>
</div>
