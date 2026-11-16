/** Shared dashboard transport. Each view owns its response-specific error copy. */
export async function dashboardRequest(
	path: string,
	init: RequestInit | undefined,
	onError: (response: Response) => Error | Promise<Error>
): Promise<any> {
	const response = await fetch(path, {
		...init,
		credentials: 'same-origin',
		headers: { 'Content-Type': 'application/json', ...init?.headers }
	});
	if (!response.ok) throw await onError(response);
	return response.status === 204 ? null : response.json();
}
