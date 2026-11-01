import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
	IDLE_GRACE_MS,
	LoadingIndicator,
	MIN_VISIBLE_MS,
	SHOW_DELAY_MS,
	installFetchTracking,
	quietly,
	trackFetch
} from './loading.svelte';

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe('LoadingIndicator', () => {
	it('stays hidden for work faster than the show delay', () => {
		const ind = new LoadingIndicator();
		const end = ind.begin();
		vi.advanceTimersByTime(SHOW_DELAY_MS - 50);
		end();
		vi.advanceTimersByTime(5000);
		expect(ind.visible).toBe(false);
		expect(ind.pending).toBe(0);
	});

	it('shows once work outlasts the delay and hides after the grace', () => {
		const ind = new LoadingIndicator();
		const end = ind.begin();
		vi.advanceTimersByTime(SHOW_DELAY_MS);
		expect(ind.visible).toBe(true);
		vi.advanceTimersByTime(MIN_VISIBLE_MS);
		end();
		vi.advanceTimersByTime(IDLE_GRACE_MS - 1);
		expect(ind.visible).toBe(true);
		vi.advanceTimersByTime(1);
		expect(ind.visible).toBe(false);
	});

	it('keeps a shown bar up for the minimum time', () => {
		const ind = new LoadingIndicator();
		const end = ind.begin();
		vi.advanceTimersByTime(SHOW_DELAY_MS + 10);
		end();
		vi.advanceTimersByTime(MIN_VISIBLE_MS - 20);
		expect(ind.visible).toBe(true);
		vi.advanceTimersByTime(10);
		expect(ind.visible).toBe(false);
	});

	it('counts back-to-back work as one load', () => {
		const ind = new LoadingIndicator();
		const a = ind.begin();
		vi.advanceTimersByTime(100);
		a();
		vi.advanceTimersByTime(50);
		const b = ind.begin();
		// The delay runs from the first begin(), not the second.
		vi.advanceTimersByTime(SHOW_DELAY_MS - 151);
		expect(ind.visible).toBe(false);
		vi.advanceTimersByTime(1);
		expect(ind.visible).toBe(true);
		b();
	});

	it('shows immediately when work resumes after the delay elapsed in a gap', () => {
		const ind = new LoadingIndicator();
		const a = ind.begin();
		vi.advanceTimersByTime(SHOW_DELAY_MS - 20);
		a();
		vi.advanceTimersByTime(40); // delay fires while idle → not shown
		expect(ind.visible).toBe(false);
		const b = ind.begin();
		expect(ind.visible).toBe(true);
		b();
	});

	it('forgets an elapsed delay once the grace runs out', () => {
		const ind = new LoadingIndicator();
		const a = ind.begin();
		vi.advanceTimersByTime(SHOW_DELAY_MS - 20);
		a();
		vi.advanceTimersByTime(IDLE_GRACE_MS);
		const b = ind.begin();
		expect(ind.visible).toBe(false);
		vi.advanceTimersByTime(SHOW_DELAY_MS);
		expect(ind.visible).toBe(true);
		b();
	});

	it('cancels a pending hide when new work starts', () => {
		const ind = new LoadingIndicator();
		const a = ind.begin();
		vi.advanceTimersByTime(SHOW_DELAY_MS + MIN_VISIBLE_MS);
		a();
		const b = ind.begin();
		vi.advanceTimersByTime(5000);
		expect(ind.visible).toBe(true);
		b();
		vi.advanceTimersByTime(IDLE_GRACE_MS);
		expect(ind.visible).toBe(false);
	});

	it('waits for every slot and releases each once', () => {
		const ind = new LoadingIndicator();
		const a = ind.begin();
		const b = ind.begin();
		a();
		a();
		expect(ind.pending).toBe(1);
		vi.advanceTimersByTime(SHOW_DELAY_MS);
		expect(ind.visible).toBe(true);
		b();
		expect(ind.pending).toBe(0);
	});
});

const ORIGIN = 'https://cairn.test';

function deferred<T>() {
	let resolve!: (v: T) => void;
	let reject!: (e: unknown) => void;
	const promise = new Promise<T>((res, rej) => {
		resolve = res;
		reject = rej;
	});
	return { promise, resolve, reject };
}

describe('trackFetch', () => {
	it('holds a slot until a same-origin response arrives', async () => {
		const ind = new LoadingIndicator();
		const d = deferred<Response>();
		const f = trackFetch(() => d.promise, ORIGIN, ind);
		const p = f('/api/x');
		expect(ind.pending).toBe(1);
		d.resolve(new Response('ok'));
		await p;
		expect(ind.pending).toBe(0);
	});

	it('tracks absolute same-origin URLs, URL objects and Requests', async () => {
		const ind = new LoadingIndicator();
		const seen: number[] = [];
		const f = trackFetch(
			async () => {
				seen.push(ind.pending);
				return new Response('');
			},
			ORIGIN,
			ind
		);
		await f(`${ORIGIN}/a`);
		await f(new URL('/b', ORIGIN));
		await f(new Request(`${ORIGIN}/c`));
		expect(seen).toEqual([1, 1, 1]);
	});

	it('ignores cross-origin and unparseable URLs', async () => {
		const ind = new LoadingIndicator();
		const seen: number[] = [];
		const base = vi.fn(async () => {
			seen.push(ind.pending);
			return new Response('');
		});
		const f = trackFetch(base, ORIGIN, ind);
		await f('https://tiles.example.org/style.json');
		await f('//tiles.example.org/x');
		await f('http://');
		expect(seen).toEqual([0, 0, 0]);
		expect(base).toHaveBeenCalledTimes(3);
	});

	it('keeps the slot while the body is read', async () => {
		const ind = new LoadingIndicator();
		const f = trackFetch(async () => new Response('{"a":1}'), ORIGIN, ind);
		const res = await f('/api/heatmap');
		expect(ind.pending).toBe(0);
		const body = res.json();
		expect(ind.pending).toBe(1);
		expect(await body).toEqual({ a: 1 });
		expect(ind.pending).toBe(0);
	});

	it('releases on a failed body read', async () => {
		const ind = new LoadingIndicator();
		const f = trackFetch(async () => new Response('not json'), ORIGIN, ind);
		const res = await f('/api/x');
		await expect(res.json()).rejects.toThrow();
		expect(ind.pending).toBe(0);
	});

	it('skips readers the response lacks', async () => {
		const ind = new LoadingIndicator();
		const fake = { text: async () => 'hi' } as unknown as Response;
		const f = trackFetch(async () => fake, ORIGIN, ind);
		const res = await f('/x');
		expect(await res.text()).toBe('hi');
		expect('json' in res).toBe(false);
	});

	it('releases when the request fails', async () => {
		const ind = new LoadingIndicator();
		const f = trackFetch(
			async () => {
				throw new TypeError('network');
			},
			ORIGIN,
			ind
		);
		await expect(f('/api/x')).rejects.toThrow('network');
		expect(ind.pending).toBe(0);
	});

	it('does not track requests started inside quietly()', async () => {
		const ind = new LoadingIndicator();
		const seen: number[] = [];
		const f = trackFetch(
			async () => {
				seen.push(ind.pending);
				return new Response('');
			},
			ORIGIN,
			ind
		);
		const out = await quietly(async () => {
			await f('/api/accounts');
			return 'done';
		});
		expect(out).toBe('done');
		await expect(
			quietly(async () => {
				throw new Error('boom');
			})
		).rejects.toThrow('boom');
		await f('/api/accounts');
		expect(seen).toEqual([0, 1]);
	});
});

describe('installFetchTracking', () => {
	it('wraps the target fetch once, keeping its receiver', async () => {
		const target = {
			location: { origin: ORIGIN },
			fetch: async function (this: unknown) {
				expect(this).toBe(target);
				return new Response('');
			} as typeof fetch
		};
		const original = target.fetch;
		installFetchTracking(target);
		const wrapped = target.fetch;
		expect(wrapped).not.toBe(original);
		installFetchTracking(target);
		expect(target.fetch).toBe(wrapped);
		await target.fetch('/x');
	});
});
