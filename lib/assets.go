package lib

import _ "embed"

// The ESM file is the byte-for-byte @serenity-kit/opaque@1.1.0 Ristretto build.
// Its package-level MIT notice is retained beside the embedded asset.
//
//go:embed assets/opaque.js
var opaqueAsset []byte

//go:embed assets/register.js
var registerScript []byte
