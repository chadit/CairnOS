package api_test

import (
	"github.com/chadit/CairnOS/internal/api"
	"github.com/chadit/CairnOS/internal/nettest"
)

// The suite serves every fixture on nettest.Network, so it has to keep
// satisfying the server's transport contract.
var _ api.Transport = (*nettest.Network)(nil)
