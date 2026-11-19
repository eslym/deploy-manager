import {
	beginSetupGrantSanitizing,
	clearSetupGrant,
	getSetupGrant,
	markSetupGrantReady
} from './setup-grant.svelte.js';
import { rememberSetupGrant } from './setup-grant.svelte.js';
import { BrowserSDKError, sdk } from './sdk/index.js';

export class CredentialFlowError extends Error {}

export function createCredentialFlow(purpose: 'registration' | 'recovery') {
	const state = $state({ pending: false, complete: false, statusMessage: '' });

	return {
		state,
		hasGrant() {
			return Boolean(getSetupGrant());
		},
		intakeGrant(fragment: string) {
			if (fragment) rememberSetupGrant(fragment);
		},
		sanitizeURL(
			target: URL | undefined,
			navigate: (url: URL) => Promise<unknown>,
			currentURL: () => string
		) {
			if (
				!target ||
				target.pathname !== (purpose === 'registration' ? '/register' : '/recover') ||
				!target.hash
			)
				return;
			if (!getSetupGrant() || !beginSetupGrantSanitizing()) return;

			const cleanURL = new URL(target);
			cleanURL.hash = '';
			void navigate(cleanURL)
				.then(() => {
					if (currentURL() === cleanURL.href) markSetupGrantReady();
					else clearSetupGrant();
				})
				.catch(clearSetupGrant);
		},
		async submit(passwordField: HTMLInputElement) {
			if (state.pending || state.complete) return;
			const grant = getSetupGrant();
			if (!grant) {
				passwordField.value = '';
				state.statusMessage = `This ${purpose === 'registration' ? 'setup' : 'recovery'} link is incomplete. Ask an administrator to issue a new one.`;
				return;
			}
			let password = passwordField.value;
			passwordField.value = '';
			if (password.length < 12) {
				password = '';
				state.statusMessage = 'Use a password with at least 12 characters.';
				return;
			}
			state.pending = true;
			state.statusMessage =
				purpose === 'registration'
					? 'Creating your account securely…'
					: 'Replacing the administrator password securely…';
			try {
				await registerCredential(password, grant, purpose);
				state.complete = true;
				state.statusMessage =
					purpose === 'registration'
						? 'Administrator account created. You can close this page.'
						: 'Administrator password replaced. You can close this page.';
			} catch (cause) {
				state.statusMessage =
					cause instanceof CredentialFlowError
						? cause.message
						: `Secure ${purpose === 'registration' ? 'registration' : 'recovery'} could not be initialized or completed. Check that JavaScript and WebAssembly are enabled, then reload and try again.`;
			} finally {
				password = '';
				passwordField.value = '';
				state.pending = false;
			}
		}
	};
}

export async function registerCredential(
	password: string,
	grant: string,
	purpose: 'registration' | 'recovery'
) {
	try {
		const opaque = await import('@serenity-kit/opaque');
		await opaque.ready;
		if (typeof opaque.client?.startRegistration !== 'function') {
			throw new CredentialFlowError(
				`Secure ${purpose} could not start. Check that JavaScript and WebAssembly are enabled, then reload this page.`
			);
		}

		const profile = { 'argon2id-custom': { iterations: 1, memory: 65536, parallelism: 4 } };
		const start = opaque.client.startRegistration({ password });
		const response = await send(
			() => sdk.auth.registerStart({ grant, value: start.registrationRequest }),
			purpose
		);
		if (
			!response ||
			typeof response.value !== 'string' ||
			typeof response.transaction !== 'string' ||
			response.transaction.length === 0
		) {
			throw new CredentialFlowError(
				`${purpose === 'registration' ? 'Registration' : 'Recovery'} could not be completed. Ask an administrator for a new ${purpose === 'registration' ? 'setup' : 'recovery'} link.`
			);
		}
		const finish = opaque.client.finishRegistration({
			password,
			registrationResponse: response.value,
			clientRegistrationState: start.clientRegistrationState,
			keyStretching: profile
		});
		password = '';
		const finishRegistration = {
			transaction: response.transaction,
			value: finish.registrationRecord
		};
		if (purpose === 'recovery') {
			await send(() => sdk.auth.recoveryFinish(finishRegistration), purpose);
		} else {
			await send(() => sdk.auth.registerFinish(finishRegistration), purpose);
		}
		clearSetupGrant();
	} finally {
		password = '';
	}
}

async function send<T>(
	request: () => Promise<{ data: T }>,
	purpose: 'registration' | 'recovery'
): Promise<T> {
	try {
		return (await request()).data;
	} catch (error) {
		if (error instanceof BrowserSDKError && error.outcome.kind === 'server') {
			if (error.outcome.code === 'auth.registration_expired') {
				throw new CredentialFlowError(
					`This ${purpose === 'registration' ? 'setup' : 'recovery'} link has expired or was already used. Ask an administrator for a new one.`
				);
			}
		}
		throw new CredentialFlowError(
			`${purpose === 'registration' ? 'Registration' : 'Recovery'} could not be completed. Check your connection and try again.`
		);
	}
}
