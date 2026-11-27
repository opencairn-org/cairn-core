<script lang="ts">
	import type { PageData } from './$types';

	let { data }: { data: PageData } = $props();
</script>

<section class="space-y-6">
	<header class="flex items-baseline justify-between">
		<div>
			<a href="/admin" class="text-xs text-accent-400 hover:text-accent-300">← back</a>
			<h1 class="mt-2 text-3xl font-semibold tracking-tight max-md:text-2xl">Identity Providers</h1>
			<p class="mt-1 text-sm text-zinc-400">
				OIDC providers are configured entirely through <code class="text-zinc-300">
					CAIRN_OIDC_*
				</code> environment variables and shown on the login page. This view is read-only — to add, change
				or remove a provider, edit the environment and restart the server.
			</p>
		</div>
	</header>

	<section class="rounded-lg border border-zinc-800 bg-zinc-900/40">
		<header
			class="border-b border-zinc-800 px-5 py-3 text-xs uppercase tracking-wide text-zinc-500"
		>
			Configured providers
		</header>
		<ul class="divide-y divide-zinc-800">
			{#each data.oidcClients as c (c.id)}
				<li
					class="grid grid-cols-[auto_1fr_1fr_auto] items-center gap-4 px-5 py-3 text-sm max-md:grid-cols-[auto_1fr] max-md:gap-3"
				>
					<span
						class="flex h-6 w-6 items-center justify-center rounded bg-accent-500/20 text-[10px] font-bold text-accent-300"
					>
						{c.displayName.charAt(0).toUpperCase()}
					</span>
					<div>
						<div class="font-medium">{c.displayName}</div>
						<div class="text-xs text-zinc-500 max-md:break-all">{c.issuerURL}</div>
						<div class="mt-0.5 font-mono text-[10px] text-zinc-600">id: {c.id}</div>
					</div>
					<div class="font-mono text-xs text-zinc-500 max-md:break-all">{c.clientID}</div>
					<div class="flex items-center gap-1 text-[10px] uppercase max-md:flex-wrap">
						{#if c.autoProvision}
							<span class="rounded bg-accent-500/20 px-1.5 py-0.5 text-accent-300">auto</span>
						{/if}
						{#if c.usePKCE}
							<span class="rounded bg-violet-500/20 px-1.5 py-0.5 text-violet-300">pkce</span>
						{/if}
						{#if c.skipAudienceCheck}
							<span class="rounded bg-amber-500/20 px-1.5 py-0.5 text-amber-300">no-aud</span>
						{/if}
						{#if c.hasClientSecret}
							<span class="rounded bg-sky-500/20 px-1.5 py-0.5 text-sky-300">secret</span>
						{/if}
					</div>
				</li>
			{:else}
				<li class="px-5 py-6 text-center text-sm text-zinc-500">
					No OIDC providers configured. Set <code>CAIRN_OIDC_PROVIDERS</code> and the per-provider
					<code>CAIRN_OIDC_&lt;ID&gt;_*</code> variables to enable SSO.
				</li>
			{/each}
		</ul>
	</section>
</section>
