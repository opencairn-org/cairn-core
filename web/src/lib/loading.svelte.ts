// Global "something is happening" indicator.
//
// Work holds a slot via `loading.begin()`; the bar (LoadingBar.svelte) renders
// `loading.visible`. Every same-origin fetch takes a slot automatically once
// installFetchTracking() has run (hooks.client.ts) — that covers raw fetch,
// Connect-RPC (it rides fetch) and load functions — and LoadingBar holds one
// per navigation. Nothing per request: new code is covered by default.
//
// Timing follows the usual perceived-performance thresholds: under ~300 ms a
// response feels instant and an indicator is just flicker, so the bar only
// appears once work has been pending for SHOW_DELAY_MS. Once shown it stays
// at least MIN_VISIBLE_MS so it never blinks, and back-to-back requests
// (await A; await B — or headers, then body) within IDLE_GRACE_MS count as
// one load rather than restarting the delay.

export const SHOW_DELAY_MS = 300;
export const IDLE_GRACE_MS = 150;
export const MIN_VISIBLE_MS = 500;

type Timer = ReturnType<typeof setTimeout>;

export class LoadingIndicator {
	visible = $state(false);

	#pending = 0;
	#shownAt = 0;
	// The show delay elapsed during an idle grace; the next begin() shows at once.
	#overdue = false;
	#showTimer: Timer | undefined;
	#idleTimer: Timer | undefined;

	get pending(): number {
		return this.#pending;
	}

	// Takes a slot; the returned function releases it (idempotent).
	begin(): () => void {
		this.#pending++;
		clearTimeout(this.#idleTimer);
		this.#idleTimer = undefined;
		if (!this.visible) {
			if (this.#overdue) this.#show();
			else this.#showTimer ??= setTimeout(() => this.#delayElapsed(), SHOW_DELAY_MS);
		}
		let released = false;
		return () => {
			if (released) return;
			released = true;
			if (--this.#pending > 0) return;
			const remaining = this.visible ? MIN_VISIBLE_MS - (Date.now() - this.#shownAt) : 0;
			this.#idleTimer = setTimeout(() => this.#settle(), Math.max(IDLE_GRACE_MS, remaining));
		};
	}

	#delayElapsed() {
		this.#showTimer = undefined;
		if (this.#pending > 0) this.#show();
		else this.#overdue = true;
	}

	#show() {
		this.#overdue = false;
		this.#shownAt = Date.now();
		this.visible = true;
	}

	#settle() {
		this.#idleTimer = undefined;
		clearTimeout(this.#showTimer);
		this.#showTimer = undefined;
		this.#overdue = false;
		this.visible = false;
	}
}

export const loading = new LoadingIndicator();

// Requests started while a quietly() callback runs don't drive the indicator —
// for background polling, which shouldn't flash the bar every few seconds.
// The browser has no async context, so this is a window, not a scope: an
// unrelated request started meanwhile goes untracked too. Keep it short.
let quietDepth = 0;

export async function quietly<T>(work: () => Promise<T>): Promise<T> {
	quietDepth++;
	try {
		return await work();
	} finally {
		quietDepth--;
	}
}

// fetch resolves on headers; a large body (the heatmap) can take far longer
// to arrive. Reading it through one of these keeps a slot held until done.
const BODY_READERS = ['arrayBuffer', 'blob', 'bytes', 'formData', 'json', 'text'] as const;

function holdWhileReading(res: Response, indicator: LoadingIndicator) {
	const r = res as unknown as Record<string, () => Promise<unknown>>;
	for (const name of BODY_READERS) {
		const read = r[name];
		if (typeof read !== 'function') continue;
		r[name] = () => read.call(res).finally(indicator.begin());
	}
}

function isSameOrigin(input: RequestInfo | URL, origin: string): boolean {
	const href = typeof input === 'string' ? input : 'href' in input ? input.href : input.url;
	try {
		return new URL(href, origin).origin === origin;
	} catch {
		return false;
	}
}

// Wraps fetch so same-origin requests hold a slot until their response
// (including a body read through a reader) is in. Cross-origin requests —
// map styles, tiles, glyphs — pass through untouched.
export function trackFetch(
	base: typeof fetch,
	origin: string,
	indicator: LoadingIndicator = loading
): typeof fetch {
	return async (input, init) => {
		if (quietDepth > 0 || !isSameOrigin(input, origin)) return base(input, init);
		const release = indicator.begin();
		try {
			const res = await base(input, init);
			holdWhileReading(res, indicator);
			return res;
		} finally {
			release();
		}
	};
}

const TRACKED = Symbol.for('cairn.loading.tracked');

// Replaces the global fetch with the tracking wrapper. Idempotent.
export function installFetchTracking(
	target: { fetch: typeof fetch; location: { origin: string } } = window
): void {
	if ((target.fetch as unknown as Record<symbol, boolean>)[TRACKED]) return;
	const tracked = trackFetch(target.fetch.bind(target), target.location.origin);
	Object.defineProperty(tracked, TRACKED, { value: true });
	target.fetch = tracked;
}
