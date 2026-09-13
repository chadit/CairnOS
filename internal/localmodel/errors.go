// Package localmodel manages the local LLM CairnOS can run for itself: a
// pinned llama.cpp llama-server build and a pinned GGUF model, both fetched
// over plain HTTPS and verified against compiled-in SHA-256 digests.
//
// Nothing here loads a model in-process. llama-server runs as its own
// supervised process and speaks the OpenAI chat-completions API, so the
// existing openai runner talks to it with only a base_url change.
package localmodel

import "errors"

// ErrUnknownModel means a name does not resolve in the built-in catalog.
var ErrUnknownModel = errors.New("localmodel: unknown model name; not in the built-in catalog")

// ErrUnsupportedPlatform means no llama.cpp release asset is pinned for this
// GOOS/GOARCH pair, so there is nothing to download.
var ErrUnsupportedPlatform = errors.New("localmodel: no pinned llama.cpp build for this platform")

// ErrOffline means a needed file is not cached and offline mode forbids the
// network, so setup cannot proceed without dropping --offline.
var ErrOffline = errors.New("localmodel: required file not cached and offline mode is set")

// ErrHashMismatch means a file's SHA-256 did not match its pinned digest. The
// caller adds the expected and actual digests by wrapping this.
var ErrHashMismatch = errors.New("localmodel: file failed SHA-256 verification")

// ErrUnexpectedStatus means an HTTP request returned a non-200 status. The
// status code is added by the caller wrapping this sentinel.
var ErrUnexpectedStatus = errors.New("localmodel: request returned unexpected HTTP status")

// ErrPathIsDir means a download destination resolved to a directory, not a
// file, so it cannot be hashed or replaced.
var ErrPathIsDir = errors.New("localmodel: destination path is a directory, not a file")

// ErrServerMissing means llama-server is not installed where the layout
// expects it. Commands surface this as a "run cairnos model setup" message.
var ErrServerMissing = errors.New("localmodel: llama-server not installed; run `cairnos model setup`")

// ErrModelMissing means the configured GGUF is absent on disk.
var ErrModelMissing = errors.New("localmodel: model file not installed; run `cairnos model setup`")

// ErrNoServerInArchive means a downloaded release archive carried no
// llama-server member, so the pin names an asset that cannot be installed.
var ErrNoServerInArchive = errors.New("localmodel: release archive carries no llama-server binary")

// ErrArchiveMember means an archive entry could not be written: an unreadable
// member, or a name that escapes the destination directory.
var ErrArchiveMember = errors.New("localmodel: cannot install archive member")

// ErrNoRelease means the llama.cpp releases endpoint answered without a usable
// stable tag, so the latest version could not be resolved.
var ErrNoRelease = errors.New("localmodel: llama.cpp releases endpoint returned no stable tag")

// ErrNoBuildTag means a stable release carried no nightly-tag.txt asset, so the
// build that holds the binaries could not be resolved.
var ErrNoBuildTag = errors.New("localmodel: stable release carries no nightly-tag.txt asset")

// ErrFileNotInRepo means a Hugging Face repository listing did not include the
// pinned file, so its upstream digest could not be compared.
var ErrFileNotInRepo = errors.New("localmodel: pinned file is absent from the upstream repository listing")

// ErrServerUnhealthy means llama-server did not answer /v1/models within the
// probe deadline, so an upgrade cannot be declared good.
var ErrServerUnhealthy = errors.New("localmodel: llama-server did not answer within the probe timeout")
