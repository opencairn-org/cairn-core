<script lang="ts">
	let email = $state('');
	let busy = $state(false);
	let done = $state(false);
	let message = $state<string | null>(null);

	async function submit(e: Event) {
		e.preventDefault();
		busy = true;
		try {
			const res = await fetch('/auth/password/forgot', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ email: email.trim() })
			});
			const body = await res.json().catch(() => ({}));
			message = body.message ?? 'If that email belongs to an account, a reset link is on its way.';
			done = true;
		} catch {
			// Even on error, show the generic message (no enumeration).
			message = 'If that email belongs to an account, a reset link is on its way.';
			done = true;
		} finally {
			busy = false;
		}
	}
</script>

<div class="mx-auto flex min-h-[60vh] max-w-md flex-col justify-center space-y-6">
	<header class="text-center">
		<div class="text-xs uppercase tracking-widest text-accent-400">Cairn</div>
		<h1 class="mt-2 text-2xl font-semibold tracking-tight">Reset your password</h1>
		<p class="mt-1 text-sm text-zinc-400">We'll email you a link to choose a new one.</p>
	</header>

	{#if done}
		<div
			class="rounded-lg border border-emerald-700/50 bg-emerald-950/30 px-4 py-3 text-sm text-emerald-300"
		>
			{message}
		</div>
		<a href="/login" class="text-center text-xs text-zinc-500 hover:text-accent-300">
			Back to sign in
		</a>
	{:else}
		<form onsubmit={submit} class="space-y-3 rounded-lg border border-zinc-800 bg-zinc-900/40 p-4">
			<label class="block">
				<span class="text-xs text-zinc-400">Email</span>
				<input
					bind:value={email}
					type="email"
					autocomplete="email"
					required
					class="mt-1 w-full rounded border border-zinc-700 bg-zinc-950 px-3 py-2 text-sm text-zinc-100 focus:border-accent-500 focus:outline-none"
				/>
			</label>
			<button
				type="submit"
				disabled={busy || !email.trim()}
				class="w-full rounded bg-accent-500 px-3 py-2 text-sm font-medium text-zinc-950 hover:bg-accent-400 disabled:opacity-50"
			>
				{busy ? 'Sending…' : 'Send reset link'}
			</button>
			<div class="text-center">
				<a href="/login" class="text-xs text-zinc-500 hover:text-accent-300">Back to sign in</a>
			</div>
		</form>
	{/if}
</div>
