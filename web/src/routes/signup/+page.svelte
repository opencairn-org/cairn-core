<script lang="ts">
	import { page } from '$app/state';
	import { refreshAll, goto } from '$app/navigation';

	let code = $state(page.url.searchParams.get('code') ?? '');
	let username = $state('');
	let email = $state('');
	let password = $state('');
	let displayName = $state('');
	let busy = $state(false);
	let error = $state<string | null>(null);

	const inputCls =
		'mt-1 w-full rounded border border-zinc-700 bg-zinc-950 px-3 py-2 text-sm text-zinc-100 focus:border-accent-500 focus:outline-none';

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = null;
		try {
			const res = await fetch('/auth/invite/redeem', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({
					code: code.trim(),
					username: username.trim(),
					email: email.trim(),
					password,
					display_name: displayName.trim()
				})
			});
			if (!res.ok) throw new Error((await res.text()).trim() || 'Sign-up failed.');
			await refreshAll();
			await goto('/');
		} catch (err) {
			error = (err as Error).message;
		} finally {
			busy = false;
		}
	}
</script>

<div class="mx-auto flex min-h-[60vh] max-w-md flex-col justify-center space-y-6">
	<header class="text-center">
		<div class="text-xs uppercase tracking-widest text-accent-400">Cairn</div>
		<h1 class="mt-2 text-3xl font-semibold tracking-tight">Create your account</h1>
		<p class="mt-1 text-sm text-zinc-400">You need an invite code to join this instance.</p>
	</header>

	<form onsubmit={submit} class="space-y-3 rounded-lg border border-zinc-800 bg-zinc-900/40 p-4">
		{#if error}
			<div class="rounded border border-red-700/50 bg-red-950/30 px-3 py-2 text-xs text-red-300">
				{error}
			</div>
		{/if}
		<label class="block">
			<span class="text-xs text-zinc-400">Invite code</span>
			<input type="text" autocomplete="off" required bind:value={code} class={inputCls} />
		</label>
		<label class="block">
			<span class="text-xs text-zinc-400">Username</span>
			<input type="text" autocomplete="username" required bind:value={username} class={inputCls} />
		</label>
		<label class="block">
			<span class="text-xs text-zinc-400">Email</span>
			<input type="email" autocomplete="email" required bind:value={email} class={inputCls} />
		</label>
		<label class="block">
			<span class="text-xs text-zinc-400">Display name (optional)</span>
			<input type="text" autocomplete="name" bind:value={displayName} class={inputCls} />
		</label>
		<label class="block">
			<span class="text-xs text-zinc-400">Password</span>
			<input
				type="password"
				autocomplete="new-password"
				required
				bind:value={password}
				class={inputCls}
			/>
		</label>
		<button
			type="submit"
			disabled={busy}
			class="w-full rounded bg-accent-500 px-3 py-2 text-sm font-medium text-zinc-950 hover:bg-accent-400 disabled:opacity-50"
		>
			{busy ? 'Creating…' : 'Create account'}
		</button>
	</form>

	<p class="text-center text-xs text-zinc-500">
		Already have an account? <a href="/login" class="text-accent-400 hover:text-accent-300">
			Sign in
		</a>
	</p>
</div>
