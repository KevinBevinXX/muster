package api

import "github.com/ZacxDev/muster/internal/apitokens"

// stubAPITokens is fullyWiredServer's personal-token store: registration must
// not depend on it, and no handler is run against it.
type stubAPITokens struct{ apitokens.Store }
