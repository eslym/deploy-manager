package lib

// readyStorage owns prepared persistence. Obtaining it starts no recovery,
// cleanup, grants or workers. Only a successful preparation can release normal
// startup. Future singleton/maintenance policy belongs before this release,
// not in domain constructors or HTTP handlers.
type readyStorage struct {
	app *App
}

// New prepares storage before performing normal application recovery/startup.
// Failure returns no usable application and releases opened resources.
func New(cfg Config) (*App, error) {
	storage, err := prepareStorage(cfg)
	if err != nil {
		return nil, err
	}
	return storage.start()
}
