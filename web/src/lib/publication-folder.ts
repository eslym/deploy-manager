// Client-side guidance only. The Go API remains authoritative for validation and reservations.
export const publicationFolderPattern = '(?!(?:[0-9a-f]{32})$)[A-Za-z0-9][A-Za-z0-9._-]*';

export function isReservedPublicationFolder(folder: string): boolean {
	return /^[0-9a-f]{32}$/.test(folder);
}

export function isValidPublicationFolder(folder: string): boolean {
	return (
		folder.length <= 100 &&
		/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(folder) &&
		!isReservedPublicationFolder(folder)
	);
}

export function publicationFolderPreview(root: string, folder: string): string {
	return root && isValidPublicationFolder(folder) ? `${root.replace(/\/+$/, '')}/${folder}` : '';
}
