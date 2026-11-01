import type { ClientInit } from '@sveltejs/kit/hooks';
import { installFetchTracking } from '#lib/loading.svelte.js';

// Runs before the first navigation, so every request — load functions
// included — feeds the global loading indicator.
export const init: ClientInit = () => {
	installFetchTracking();
};
