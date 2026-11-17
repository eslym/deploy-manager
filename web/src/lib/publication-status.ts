/** A failed deployment list must not leave previously loaded publication paths visible. */
export class PublicationStatusUnavailableError extends Error {}

export async function publicationStatusUnavailable(response: Response) {
	const detail = await response.text();
	return new PublicationStatusUnavailableError(
		/publication move unresolved/i.test(detail)
			? 'A publication move needs recovery. The document root cannot be trusted until recovery is complete. Ask an administrator to recover the service.'
			: 'Publication status is unavailable. Do not rely on a previously shown document root until status can be verified. Try again shortly.'
	);
}
