import {
	beginSetupGrantSanitizing,
	clearSetupGrant,
	getSetupGrant,
	markSetupGrantReady
} from './setup-grant.svelte.js';

export class RegistrationError extends Error {}

export function createRegistrationFlow() {
	const state = $state({ pending: false, complete: false, error: '' });

	return Object.assign(state, {
		async submit(passwordField: HTMLInputElement) {
			if (state.pending || state.complete) return;
			const grant = getSetupGrant();
			if (!grant) {
				state.error = 'This setup link is incomplete. Ask an administrator to issue a new one.';
				return;
			}
			let password = passwordField.value;
			passwordField.value = '';
			if (password.length < 12) {
				password = '';
				state.error = 'Use a password with at least 12 characters.';
				return;
			}
			state.pending = true;
			state.error = 'Creating your account securely…';
			try {
				await registerCredential(password, grant);
				state.complete = true;
				state.error = 'Administrator account created. You can close this page.';
			} catch (cause) {
				state.error =
					cause instanceof RegistrationError
						? cause.message
						: 'Secure registration could not be initialized or completed. Check that JavaScript and WebAssembly are enabled, then reload and try again.';
			} finally {
				password = '';
				passwordField.value = '';
				state.pending = false;
			}
		}
	});
}

export function sanitizeSetupGrantURL(
	target: URL | undefined,
	navigate: (url: URL) => Promise<unknown>,
	currentURL: () => string
) {
	if (!target || target.pathname !== '/register' || !target.hash) return;
	if (!getSetupGrant() || !beginSetupGrantSanitizing()) return;

	const cleanURL = new URL(target);
	cleanURL.hash = '';
	void navigate(cleanURL)
		.then(() => {
			if (currentURL() === cleanURL.href) markSetupGrantReady();
			else clearSetupGrant();
		})
		.catch(clearSetupGrant);
}

export async function registerCredential(password: string, grant: string) {
	try {
		const opaque = await import('@serenity-kit/opaque');
		await opaque.ready;
		if (typeof opaque.client?.startRegistration !== 'function') {
			throw new RegistrationError(
				'Secure registration could not start. Check that JavaScript and WebAssembly are enabled, then reload this page.'
			);
		}

		const profile = { 'argon2id-custom': { iterations: 1, memory: 65536, parallelism: 4 } };
		const start = opaque.client.startRegistration({ password });
		const response = await send(
			'/opaque/register/start',
			start.registrationRequest,
			'grant',
			grant
		);
		if (
			!response ||
			typeof response.value !== 'string' ||
			typeof response.transaction !== 'string' ||
			response.transaction.length === 0
		) {
			throw new RegistrationError(
				'Registration could not be completed. Ask an administrator for a new setup link.'
			);
		}
		const finish = opaque.client.finishRegistration({
			password,
			registrationResponse: response.value,
			clientRegistrationState: start.clientRegistrationState,
			keyStretching: profile
		});
		await send(
			'/opaque/register/finish',
			finish.registrationRecord,
			'transaction',
			response.transaction
		);
		clearSetupGrant();
	} finally {
		password = '';
	}
}

async function send(
	path: string,
	value: string,
	credentialName: 'grant' | 'transaction',
	credential: string
): Promise<{ value: string; transaction?: string } | null> {
	const response = await fetch(path, {
		method: 'POST',
		credentials: 'same-origin',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ [credentialName]: credential, value })
	});
	if (!response.ok) {
		if (response.status === 410) {
			throw new RegistrationError(
				'This setup link has expired or was already used. Ask an administrator for a new one.'
			);
		}
		throw new RegistrationError(
			'Registration could not be completed. Check your connection and try again.'
		);
	}
	return response.status === 204 ? null : response.json();
}
