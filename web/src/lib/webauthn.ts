// WebAuthn ceremony helpers. The server (go-webauthn) emits and consumes the
// standard WebAuthn JSON; the browser's navigator.credentials works in
// ArrayBuffers, so these helpers convert base64url ↔ buffers and drive the two
// ceremonies. No external dependency.

export function supported(): boolean {
	return (
		typeof window !== 'undefined' &&
		!!window.PublicKeyCredential &&
		typeof navigator.credentials?.create === 'function'
	);
}

function b64urlToBuf(s: string): ArrayBuffer {
	const pad = s.length % 4 === 0 ? '' : '='.repeat(4 - (s.length % 4));
	const b64 = (s + pad).replace(/-/g, '+').replace(/_/g, '/');
	const bin = atob(b64);
	const bytes = new Uint8Array(bin.length);
	for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
	return bytes.buffer;
}

function bufToB64url(buf: ArrayBuffer): string {
	const bytes = new Uint8Array(buf);
	let bin = '';
	for (const b of bytes) bin += String.fromCharCode(b);
	return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// Server JSON shapes: binary fields arrive as base64url strings.
type DescriptorJSON = Omit<PublicKeyCredentialDescriptor, 'id'> & { id: string };
type CreationOptionsJSON = Omit<
	PublicKeyCredentialCreationOptions,
	'challenge' | 'user' | 'excludeCredentials'
> & {
	challenge: string;
	user: Omit<PublicKeyCredentialUserEntity, 'id'> & { id: string };
	excludeCredentials?: DescriptorJSON[];
};
type RequestOptionsJSON = Omit<
	PublicKeyCredentialRequestOptions,
	'challenge' | 'allowCredentials'
> & {
	challenge: string;
	allowCredentials?: DescriptorJSON[];
};

// Convert the server's CredentialCreationOptions JSON (with base64url strings)
// into the ArrayBuffer form navigator.credentials.create expects.
function decodeCreationOptions(pk: CreationOptionsJSON): PublicKeyCredentialCreationOptions {
	return {
		...pk,
		challenge: b64urlToBuf(pk.challenge),
		user: { ...pk.user, id: b64urlToBuf(pk.user.id) },
		excludeCredentials: (pk.excludeCredentials ?? []).map((c) => ({
			...c,
			id: b64urlToBuf(c.id)
		}))
	};
}

function decodeRequestOptions(pk: RequestOptionsJSON): PublicKeyCredentialRequestOptions {
	return {
		...pk,
		challenge: b64urlToBuf(pk.challenge),
		allowCredentials: (pk.allowCredentials ?? []).map((c) => ({
			...c,
			id: b64urlToBuf(c.id)
		}))
	};
}

/** Register a new passkey for the signed-in user. */
export async function registerPasskey(name: string): Promise<void> {
	const startRes = await fetch('/auth/webauthn/register/start', { method: 'POST' });
	if (!startRes.ok)
		throw new Error((await startRes.text()).trim() || 'could not start registration');
	const options = await startRes.json();

	const cred = (await navigator.credentials.create({
		publicKey: decodeCreationOptions(options.publicKey)
	})) as PublicKeyCredential | null;
	if (!cred) throw new Error('registration was cancelled');

	const att = cred.response as AuthenticatorAttestationResponse;
	const body = {
		id: cred.id,
		rawId: bufToB64url(cred.rawId),
		type: cred.type,
		response: {
			attestationObject: bufToB64url(att.attestationObject),
			clientDataJSON: bufToB64url(att.clientDataJSON),
			transports: att.getTransports?.() ?? []
		},
		clientExtensionResults: cred.getClientExtensionResults()
	};

	const finishRes = await fetch(`/auth/webauthn/register/finish?name=${encodeURIComponent(name)}`, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify(body)
	});
	if (!finishRes.ok) throw new Error((await finishRes.text()).trim() || 'registration failed');
}

/** Sign in with a passkey (usernameless / discoverable). */
export async function loginWithPasskey(): Promise<void> {
	await runPasskeyAssertion('/auth/webauthn/login/finish');
}

/**
 * Second factor after a password sign-in: the passkey assertion satisfies the
 * server's challenge, then the challenge is exchanged for a session.
 */
export async function confirmSecondFactor(challengeToken: string): Promise<void> {
	await runPasskeyAssertion(
		`/auth/webauthn/login/finish?challenge=${encodeURIComponent(challengeToken)}`
	);
	const res = await fetch('/auth/2fa/complete', {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ challenge_token: challengeToken })
	});
	if (!res.ok) throw new Error((await res.text()).trim() || 'sign-in could not be completed');
}

async function runPasskeyAssertion(finishURL: string): Promise<void> {
	const startRes = await fetch('/auth/webauthn/login/start', { method: 'POST' });
	if (!startRes.ok) throw new Error((await startRes.text()).trim() || 'could not start login');
	const options = await startRes.json();

	const cred = (await navigator.credentials.get({
		publicKey: decodeRequestOptions(options.publicKey)
	})) as PublicKeyCredential | null;
	if (!cred) throw new Error('login was cancelled');

	const asr = cred.response as AuthenticatorAssertionResponse;
	const body = {
		id: cred.id,
		rawId: bufToB64url(cred.rawId),
		type: cred.type,
		response: {
			authenticatorData: bufToB64url(asr.authenticatorData),
			clientDataJSON: bufToB64url(asr.clientDataJSON),
			signature: bufToB64url(asr.signature),
			userHandle: asr.userHandle ? bufToB64url(asr.userHandle) : null
		},
		clientExtensionResults: cred.getClientExtensionResults()
	};

	const finishRes = await fetch(finishURL, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify(body)
	});
	if (!finishRes.ok) throw new Error((await finishRes.text()).trim() || 'login failed');
}
