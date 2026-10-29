<script lang="ts">
	import { page } from '$app/state';
	import { refreshAll, goto } from '$app/navigation';
	import {
		confirmSecondFactor,
		loginWithPasskey,
		supported as passkeysSupported
	} from '#lib/webauthn.js';
	import type { PageData } from './$types';

	let { data }: { data: PageData } = $props();

	// Post-login destination (e.g. the OAuth consent page). Constrained to a
	// local path; the password endpoint applies the same guard server-side.
	const nextDest = $derived.by(() => {
		const n = page.url.searchParams.get('next') ?? '';
		return n.startsWith('/') && !n.startsWith('//') ? n : '/';
	});

	// A password sign-in that still needs its passkey lands here with ?challenge=.
	const challenge = $derived(page.url.searchParams.get('challenge') ?? '');
	let challengeBusy = $state(false);
	let challengeError = $state<string | null>(null);
	async function finishSecondFactor() {
		challengeBusy = true;
		challengeError = null;
		try {
			await confirmSecondFactor(challenge);
			await refreshAll();
			await goto(nextDest).catch(() => location.assign(nextDest));
		} catch (e) {
			challengeError = (e as Error).message;
		} finally {
			challengeBusy = false;
		}
	}

	let passkeyBusy = $state(false);
	let passkeyError = $state<string | null>(null);
	async function signInWithPasskey() {
		passkeyBusy = true;
		passkeyError = null;
		try {
			await loginWithPasskey();
			await refreshAll();
			// goto rejects non-SvelteKit paths; fall back to a full load.
			await goto(nextDest).catch(() => location.assign(nextDest));
		} catch (e) {
			passkeyError = (e as Error).message;
		} finally {
			passkeyBusy = false;
		}
	}

	// The password + OIDC endpoints bounce back to /login?error=<code> on failure.
	const errorMessages: Record<string, string> = {
		invalid_credentials: 'Incorrect username/email or password.',
		missing_credentials: 'Enter your username/email and password.',
		account_inactive: 'This account is not active.',
		server_error: 'Something went wrong — please try again.',
		// OIDC / SSO sign-in failures.
		oidc_expired: 'Your sign-in took too long or was interrupted. Please try again.',
		oidc_state: 'Sign-in could not be verified. Please start again.',
		oidc_verify_failed: 'The identity provider’s response could not be verified.',
		oidc_bad_response: 'The identity provider returned an incomplete response.',
		oidc_unavailable: 'That identity provider is not available.',
		auto_provision_denied:
			'No Cairn account exists for this identity yet — ask an admin to invite you.',
		oidc_failed: 'Single sign-on failed. Please try again.'
	};
	const loginError = $derived(
		page.url.searchParams.get('error')
			? (errorMessages[page.url.searchParams.get('error')!] ?? 'Sign-in failed.')
			: null
	);

	// Recovery-flow notices.
	const notice = $derived(
		page.url.searchParams.get('verified')
			? 'Email verified — you can sign in now.'
			: page.url.searchParams.get('reset')
				? 'Password updated — sign in with your new password.'
				: null
	);
	const verifyError = $derived(
		page.url.searchParams.get('verify_error')
			? 'That verification link is invalid or has expired.'
			: null
	);
</script>

<div class="mx-auto flex min-h-[60vh] max-w-md flex-col justify-center space-y-8">
	<header class="text-center">
		<div class="text-xs uppercase tracking-widest text-accent-400">Cairn</div>
		<h1 class="mt-2 text-3xl font-semibold tracking-tight">Sign in</h1>
		<p class="mt-1 text-sm text-zinc-400">Self-hosted activity tracker.</p>
	</header>

	{#if notice}
		<div
			class="rounded-lg border border-emerald-700/50 bg-emerald-950/30 px-4 py-3 text-sm text-emerald-300"
		>
			{notice}
		</div>
	{/if}
	{#if verifyError}
		<div class="rounded-lg border border-red-700/50 bg-red-950/30 px-4 py-3 text-sm text-red-300">
			{verifyError}
		</div>
	{/if}

	{#if challenge}
		<section class="space-y-3 rounded-lg border border-zinc-800 bg-zinc-900/40 p-4">
			<h2 class="text-sm font-medium text-zinc-200">Confirm it's you</h2>
			<p class="text-xs text-zinc-400">
				Your account requires a passkey after the password. Use Touch ID, Windows Hello or your
				security key to finish signing in.
			</p>
			<button
				type="button"
				onclick={finishSecondFactor}
				disabled={challengeBusy || !passkeysSupported()}
				class="w-full rounded bg-accent-500 px-3 py-2 text-sm font-medium text-zinc-950 hover:bg-accent-400 disabled:opacity-50"
			>
				{challengeBusy ? 'Waiting for passkey…' : 'Continue with passkey'}
			</button>
			{#if !passkeysSupported()}
				<p class="text-xs text-red-300">This browser doesn’t support passkeys.</p>
			{/if}
			{#if challengeError}
				<div class="rounded border border-red-700/50 bg-red-950/30 px-3 py-2 text-xs text-red-300">
					{challengeError}
				</div>
			{/if}
			<a href="/login" class="block text-center text-xs text-zinc-500 hover:text-accent-300"
				>Start over</a
			>
		</section>
	{:else}
		{#if data.oidcClients.length > 0}
			<section class="space-y-2">
				{#each data.oidcClients as client (client.id)}
					<a
						href={`/auth/oidc/${client.id}/start`}
						class="flex items-center justify-between rounded-lg border border-zinc-800 bg-zinc-900/60 px-4 py-3 text-sm hover:border-accent-500 hover:bg-zinc-900"
					>
						<span class="flex items-center gap-3">
							<span
								class="flex h-5 w-5 items-center justify-center rounded bg-accent-500/20 text-[10px] font-bold text-accent-300"
							>
								{client.displayName.charAt(0).toUpperCase()}
							</span>
							<span class="font-medium">Continue with {client.displayName}</span>
						</span>
						<span class="text-zinc-500">→</span>
					</a>
				{/each}
			</section>
		{/if}

		{#if data.webauthnEnabled && passkeysSupported()}
			<section class="space-y-2">
				{#if data.oidcClients.length > 0}
					<div class="flex items-center gap-3 text-xs text-zinc-500">
						<span class="h-px flex-1 bg-zinc-800"></span>
						<span>or</span>
						<span class="h-px flex-1 bg-zinc-800"></span>
					</div>
				{/if}
				<button
					type="button"
					onclick={signInWithPasskey}
					disabled={passkeyBusy}
					class="flex w-full items-center justify-center gap-2 rounded-lg border border-zinc-800 bg-zinc-900/60 px-4 py-3 text-sm font-medium hover:border-accent-500 hover:bg-zinc-900 disabled:opacity-50"
				>
					<svg
						class="h-4 w-4"
						viewBox="0 0 24 24"
						fill="none"
						stroke="currentColor"
						stroke-width="1.8"
						stroke-linecap="round"
						stroke-linejoin="round"
					>
						<rect x="3" y="11" width="18" height="11" rx="2" />
						<path d="M7 11V7a5 5 0 0 1 10 0v4" />
					</svg>
					{passkeyBusy ? 'Waiting for passkey…' : 'Sign in with a passkey'}
				</button>
				{#if passkeyError}
					<div
						class="rounded border border-red-700/50 bg-red-950/30 px-3 py-2 text-xs text-red-300"
					>
						{passkeyError}
					</div>
				{/if}
			</section>
		{/if}

		{#if data.passwordEnabled}
			<section class="space-y-3">
				{#if data.oidcClients.length > 0 || data.webauthnEnabled}
					<div class="flex items-center gap-3 text-xs text-zinc-500">
						<span class="h-px flex-1 bg-zinc-800"></span>
						<span>or</span>
						<span class="h-px flex-1 bg-zinc-800"></span>
					</div>
				{/if}
				<form
					method="POST"
					action="/auth/password"
					class="space-y-3 rounded-lg border border-zinc-800 bg-zinc-900/40 p-4"
				>
					<input type="hidden" name="redirect" value={nextDest} />
					{#if loginError}
						<div
							class="rounded border border-red-700/50 bg-red-950/30 px-3 py-2 text-xs text-red-300"
						>
							{loginError}
						</div>
					{/if}
					<label class="block">
						<span class="text-xs text-zinc-400">Username or email</span>
						<input
							name="identifier"
							type="text"
							autocomplete="username"
							required
							class="mt-1 w-full rounded border border-zinc-700 bg-zinc-950 px-3 py-2 text-sm text-zinc-100 focus:border-accent-500 focus:outline-none"
						/>
					</label>
					<label class="block">
						<span class="text-xs text-zinc-400">Password</span>
						<input
							name="password"
							type="password"
							autocomplete="current-password"
							required
							class="mt-1 w-full rounded border border-zinc-700 bg-zinc-950 px-3 py-2 text-sm text-zinc-100 focus:border-accent-500 focus:outline-none"
						/>
					</label>
					<button
						type="submit"
						class="w-full rounded bg-accent-500 px-3 py-2 text-sm font-medium text-zinc-950 hover:bg-accent-400"
					>
						Sign in
					</button>
					<div class="text-center">
						<a href="/forgot-password" class="text-xs text-zinc-500 hover:text-accent-300"
							>Forgot password?</a
						>
					</div>
				</form>
			</section>
		{/if}

		{#if data.oidcClients.length === 0 && !data.passwordEnabled}
			<div
				class="rounded-lg border border-amber-700/50 bg-amber-950/30 p-4 text-sm text-amber-200/80"
			>
				No sign-in methods configured. Enable password sign-in, or configure an OIDC provider via
				the <code>CAIRN_OIDC_*</code> environment variables and restart the server.
			</div>
		{/if}
	{/if}

	<p class="text-center text-xs text-zinc-500">
		Have an invite code? <a href="/signup" class="text-accent-400 hover:text-accent-300"
			>Create an account</a
		>
	</p>
</div>
