package assets

import "embed"

// Helpers are embedded in the signed executable, so updates never execute
// mutable scripts fetched from the repository's default branch.
//
//go:embed scripts/update.sh scripts/remove.sh scripts/vpsagent-update.path scripts/vpsagent-update.service scripts/vpsagent-recover.service
var Files embed.FS
