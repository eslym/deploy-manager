export function modalAttachment(
	onCancel: () => void,
	isBusy: () => boolean,
	initialFocusSelector?: string
) {
	return (dialog: HTMLDialogElement) => {
		const handleCancel = (event: Event) => {
			event.preventDefault();
			if (isBusy()) return;
			dialog.close();
			onCancel();
		};

		dialog.addEventListener('cancel', handleCancel);
		dialog.showModal();
		if (initialFocusSelector) dialog.querySelector<HTMLElement>(initialFocusSelector)?.focus();

		return () => {
			dialog.removeEventListener('cancel', handleCancel);
			if (dialog.open) dialog.close();
		};
	};
}
