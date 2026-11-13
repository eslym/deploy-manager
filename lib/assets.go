package lib

import "embed"

// The combined build generates this directory from the production SPA before Go
// compilation. A missing build is a compile error, never a placeholder UI.
//
//go:embed web web/_app
var webAssets embed.FS
