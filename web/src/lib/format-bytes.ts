export function formatBytes(bytes: number): string {
	if (!Number.isFinite(bytes) || bytes <= 0) return '0 bytes';
	const units = ['bytes', 'KB', 'MB', 'GB', 'TB'];
	const unit = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
	const size = bytes / 1024 ** unit;
	return `${size.toLocaleString(undefined, { maximumFractionDigits: unit === 0 ? 0 : 1 })} ${units[unit]}`;
}
